package query

import (
	"strings"
	"testing"
)

// Every successful compile must also pass the metrics-scope validator: the
// DSL can never produce SQL its own explorer would reject.
func compileDSL(t *testing.T, input string, services []string) string {
	t.Helper()
	d, err := ParseDSL(input)
	if err != nil {
		t.Fatalf("parse %q: %v", input, err)
	}
	sql, err := d.SQL(services)
	if err != nil {
		t.Fatalf("codegen %q: %v", input, err)
	}
	if _, err := ValidateIn(sql, ScopeMetrics); err != nil {
		t.Fatalf("compiled SQL fails validation:\n%s\n%v", sql, err)
	}
	return sql
}

func TestDSLCompiles(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		services []string
		want     []string // substrings that must appear
	}{
		{
			name:  "plain avg over window is a stat",
			input: "avg(queue_depth)",
			want:  []string{"AVG(value) AS avg_value", "name = 'queue_depth'", "unixepoch() - 86400"},
		},
		{
			name:  "bucketed avg is a series",
			input: "avg(queue_depth) per 10m last 6h",
			want:  []string{"timestamp / 600000 * 600000 AS bucket", "GROUP BY bucket", "unixepoch() - 21600"},
		},
		{
			name:  "aggregate by service groups",
			input: "max(queue_depth) by service last 1h",
			want:  []string{"SELECT service, MAX(value) AS max_value", "GROUP BY service"},
		},
		{
			name:     "bucketed by service pivots with real names",
			input:    "avg(queue_depth) per 10m by service",
			services: []string{"orders-api", "payment-svc"},
			want: []string{
				`AVG(CASE WHEN service = 'orders-api' THEN value END) AS "orders-api"`,
				`AVG(CASE WHEN service = 'payment-svc' THEN value END) AS "payment-svc"`,
			},
		},
		{
			name:  "increase nests per-series MAX-MIN",
			input: "increase(http_requests_total) per 1h last 24h",
			want:  []string{"MAX(value) - MIN(value) AS inc", "GROUP BY bucket, service, host, labels", "SUM(inc) AS increase"},
		},
		{
			name:  "increase without per is a single window stat",
			input: "increase(http_requests_total) last 1h",
			want:  []string{"SUM(inc) AS increase", "GROUP BY service, host, labels"},
		},
		{
			name:  "rate divides by frame seconds",
			input: "rate(http_requests_total) per 5m",
			want:  []string{"SUM(inc) / 300.0 AS rate_per_s"},
		},
		{
			name:  "filters resolve columns vs labels",
			input: "count(http_requests_total) service=orders-api status=500 per 1h",
			want:  []string{"service = 'orders-api'", "json_extract(labels, '$.status') = '500'", "COUNT(*) AS samples"},
		},
		{
			name:  "latest scalar",
			input: "latest(queue_depth)",
			want:  []string{"ORDER BY id DESC", "LIMIT 1"},
		},
		{
			name:  "latest by service uses the max-row trick",
			input: "latest(queue_depth) by service",
			want:  []string{"MAX(id) AS latest_id", "GROUP BY service", "ORDER BY latest DESC"},
		},
		{
			name:  "quotes in names escaped",
			input: "avg(we'ird) host=h'1",
			want:  []string{"name = 'we''ird'", "host = 'h''1'"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql := compileDSL(t, tc.input, tc.services)
			for _, w := range tc.want {
				if !strings.Contains(sql, w) {
					t.Errorf("compiled SQL missing %q:\n%s", w, sql)
				}
			}
		})
	}
}

func TestDSLParseErrors(t *testing.T) {
	tests := []struct {
		input   string
		wantErr string
	}{
		{"", "start with a verb"},
		{"averg(queue_depth)", "unknown verb"},
		{"avg queue_depth", "start with a verb"},
		{"avg(q) per nope", "bad duration"},
		{"avg(q) per 0m", "can't be zero"},
		{"avg(q) per 5m per 1h", "'per' given twice"},
		{"avg(q) last", "'last' needs a duration"},
		{"avg(q) by host", "only 'by service'"},
		{"avg(q) wat", "expected key=value"},
		{"latest(q) per 5m", "doesn't take 'per'"},
	}
	for _, tc := range tests {
		if _, err := ParseDSL(tc.input); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: want error containing %q, got %v", tc.input, tc.wantErr, err)
		}
	}
}

func TestDSLNeedsServices(t *testing.T) {
	d, err := ParseDSL("avg(q) per 5m by service")
	if err != nil {
		t.Fatal(err)
	}
	if !d.NeedsServices() {
		t.Fatal("pivot must need services")
	}
	if _, err := d.SQL(nil); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("pivot without services must error, got %v", err)
	}

	for _, in := range []string{"avg(q) by service", "latest(q) by service", "avg(q) per 5m"} {
		d, err := ParseDSL(in)
		if err != nil {
			t.Fatal(err)
		}
		if d.NeedsServices() {
			t.Errorf("%q should not need services", in)
		}
	}
}

func TestDSLOverrideWindow(t *testing.T) {
	d, err := ParseDSL("avg(queue_depth) per 10m last 24h")
	if err != nil {
		t.Fatal(err)
	}
	d.OverrideWindow(1_000_000, 4_600_000)
	sql, err := d.SQL(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "timestamp BETWEEN 1000000 AND 4600000") {
		t.Errorf("override missing BETWEEN: %s", sql)
	}
	if strings.Contains(sql, "unixepoch") {
		t.Errorf("relative window survived the override: %s", sql)
	}
	if _, err := ValidateIn(sql, ScopeMetrics); err != nil {
		t.Errorf("overridden SQL fails validation: %v", err)
	}

	// rate's per-second denominator must follow the overridden span
	r, err := ParseDSL("rate(requests_total)")
	if err != nil {
		t.Fatal(err)
	}
	r.OverrideWindow(10_000, 70_000) // 60s span
	sql, err = r.SQL(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "/ 60.0") {
		t.Errorf("rate denominator ignores the override span: %s", sql)
	}
}
