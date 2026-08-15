package ui_test

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func (cl *client) htmx(method, path, token string) (int, string) {
	cl.t.Helper()
	req, err := http.NewRequest(method, cl.srv.URL+path, nil)
	if err != nil {
		cl.t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", token)
	req.Header.Set("Origin", cl.srv.URL)
	resp, err := cl.c.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func seedMetrics(cl *client) {
	cl.t.Helper()
	_, err := cl.pools.Metrics.Exec(`INSERT INTO metrics(timestamp, name, type, value, service, host, labels) VALUES
		(1723600000000, 'queue_depth', 1, 7, 'shop', 'h1', NULL),
		(1723600030000, 'queue_depth', 1, 9, 'shop', 'h1', NULL),
		(1723600000000, 'orders_total', 0, 100, 'shop', 'h1', '{"status":"ok"}')`)
	if err != nil {
		cl.t.Fatal(err)
	}
}

func TestScrapeTargetsSettingsFlow(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	status, body := cl.get("/settings")
	if status != http.StatusOK || !strings.Contains(body, "Scrape targets") {
		t.Fatalf("settings page missing scrape targets section: %d", status)
	}
	if !strings.Contains(body, "No targets yet") {
		t.Error("empty state missing")
	}
	token := csrfRe.FindStringSubmatch(body)[1]

	// create: host left blank auto-derives from the URL
	resp := cl.postForm("/settings/scrape-targets", url.Values{
		"csrf_token": {token},
		"url":        {"http://db-box.local:9187/metrics"},
		"service":    {"postgres"},
		"host":       {""},
		"interval_s": {"60"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create target = %d, want 303", resp.StatusCode)
	}
	_, body = cl.get("/settings")
	if !strings.Contains(body, "postgres") || !strings.Contains(body, "db-box.local:9187") {
		t.Error("created target not listed")
	}
	if !strings.Contains(body, "waiting") {
		t.Error("never-scraped target should show waiting status")
	}
	var host string
	if err := cl.pools.Meta.QueryRow("SELECT host FROM scrape_targets").Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host != "db-box.local" {
		t.Errorf("host not derived from URL: %q", host)
	}

	// invalid interval rejected with the section error visible
	resp = cl.postForm("/settings/scrape-targets", url.Values{
		"csrf_token": {token},
		"url":        {"http://x:1/metrics"},
		"service":    {"x"},
		"interval_s": {"5"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("5s interval = %d, want 400", resp.StatusCode)
	}

	// toggle pauses, second toggle resumes (HTMX fragment both times)
	status, frag := cl.htmx(http.MethodPost, "/settings/scrape-targets/1/toggle", token)
	if status != http.StatusOK || !strings.Contains(frag, "Resume") {
		t.Fatalf("pause toggle: %d, fragment has Resume: %v", status, strings.Contains(frag, "Resume"))
	}
	status, frag = cl.htmx(http.MethodPost, "/settings/scrape-targets/1/toggle", token)
	if status != http.StatusOK || !strings.Contains(frag, "Pause") {
		t.Fatalf("resume toggle: %d", status)
	}

	// delete
	status, frag = cl.htmx(http.MethodDelete, "/settings/scrape-targets/1", token)
	if status != http.StatusOK || !strings.Contains(frag, "No targets yet") {
		t.Fatalf("delete: %d, empty state back: %v", status, strings.Contains(frag, "No targets yet"))
	}
}

func TestMetricsExplorer(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	// default catalog query: renders as a table (a listing, never a chart)
	status, body := cl.get("/metrics-explorer")
	if status != http.StatusOK {
		t.Fatalf("explorer = %d", status)
	}
	for _, want := range []string{"queue_depth", "orders_total", "sql-editor-host"} {
		if !strings.Contains(body, want) {
			t.Errorf("explorer missing %q", want)
		}
	}
	if strings.Contains(body, "data-chart=") {
		t.Error("landing catalog must not render a chart")
	}

	// the same catalog run explicitly auto-shapes to a bar, and the epoch-ms
	// last_seen column is excluded from the series
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT name, service, COUNT(*) AS points, MAX(timestamp) AS last_seen FROM metrics GROUP BY name, service"))
	if status != http.StatusOK || !strings.Contains(body, `"type":"bar"`) {
		t.Fatalf("explicit catalog should bar-chart: %d", status)
	}
	if strings.Contains(body, `"label":"last_seen"`) {
		t.Error("epoch-ms column charted as a data series")
	}

	// custom query incl. metrics filter
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT value FROM metrics WHERE name = 'queue_depth' ORDER BY timestamp"))
	if status != http.StatusOK || !strings.Contains(body, ">7<") || !strings.Contains(body, ">9<") {
		t.Fatalf("custom query: %d", status)
	}

	// signals are isolated: logs (and thus cross-signal joins) are blocked here
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT l.raw, m.value FROM logs l JOIN metrics m ON l.service = m.service LIMIT 10"))
	if status != http.StatusBadRequest || !strings.Contains(body, "Logs page") {
		t.Fatalf("cross-signal join must be blocked on the explorer: %d", status)
	}

	// broken SQL: friendly error, 400
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT FROM WHERE"))
	if status != http.StatusBadRequest || !strings.Contains(body, "Line 1") {
		t.Fatalf("bad SQL: %d", status)
	}

	// forbidden table blocked
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT * FROM api_keys"))
	if status != http.StatusBadRequest || !strings.Contains(body, "not allowed") {
		t.Fatalf("forbidden table: %d", status)
	}

	// exports
	resp, err := cl.c.Get(cl.srv.URL + "/metrics-explorer/export?q=" + url.QueryEscape("SELECT name, value FROM metrics ORDER BY id"))
	if err != nil {
		t.Fatal(err)
	}
	csvBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(csvBody), "name,value") {
		t.Errorf("csv export header wrong: %.60s", csvBody)
	}
	resp, err = cl.c.Get(cl.srv.URL + "/metrics-explorer/export?format=json&q=" + url.QueryEscape("SELECT name FROM metrics LIMIT 1"))
	if err != nil {
		t.Fatal(err)
	}
	jsonBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(jsonBody), `"name"`) {
		t.Errorf("json export wrong: %.60s", jsonBody)
	}
}

func TestSignalIsolation(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	// logs page: metrics table blocked (rendered as the page's inline error)
	_, body := cl.get("/logs?q=" + url.QueryEscape("SELECT * FROM metrics LIMIT 5"))
	if !strings.Contains(body, "Metrics page") {
		t.Fatal("metrics on the logs page must surface the isolation error")
	}
	if strings.Contains(body, "queue_depth") {
		t.Fatal("blocked query still leaked metric data")
	}
	// logs export too
	if status, _ := cl.get("/logs/export?q=" + url.QueryEscape("SELECT * FROM metrics LIMIT 5")); status != http.StatusBadRequest {
		t.Fatalf("metrics on logs export must be blocked: %d", status)
	}
}

func TestExplorerViewsAndEmptyStates(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	// no metrics ingested at all: onboarding shows BOTH paths
	status, body := cl.get("/metrics-explorer")
	if status != http.StatusOK || !strings.Contains(body, "Two ways in") ||
		!strings.Contains(body, "/api/v1/otlp/v1/metrics") || !strings.Contains(body, "Settings") {
		t.Fatalf("onboarding empty state wrong: %d", status)
	}

	seedMetrics(cl)

	// data exists but nothing matches: plain no-match message, no onboarding
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT value FROM metrics WHERE name = 'nope'"))
	if status != http.StatusOK || !strings.Contains(body, "No rows matched") || strings.Contains(body, "Two ways in") {
		t.Fatalf("no-match empty state wrong: %d", status)
	}

	// time-series query auto-detects a line chart, rows available underneath
	timeQ := url.QueryEscape("SELECT timestamp, value FROM metrics WHERE name = 'queue_depth' ORDER BY timestamp")
	status, body = cl.get("/metrics-explorer?q=" + timeQ)
	if status != http.StatusOK || !strings.Contains(body, `"type":"line"`) || !strings.Contains(body, "labelsMs") ||
		!strings.Contains(body, "data-chart=\"explorer\"") {
		t.Fatalf("auto line chart missing: %d", status)
	}
	if !strings.Contains(body, ">7<") { // raw rows under the chart
		t.Fatal("rows table under the chart missing")
	}

	// view override forces the table
	status, body = cl.get("/metrics-explorer?view=table&q=" + timeQ)
	if status != http.StatusOK || strings.Contains(body, "data-chart=\"explorer\"") {
		t.Fatalf("table override still rendered a chart: %d", status)
	}

	// the results header carries the segmented switcher: links per view, and
	// the auto segment reports its verdict
	status, body = cl.get("/metrics-explorer?q=" + timeQ)
	if status != http.StatusOK || !strings.Contains(body, "auto (line)") {
		t.Fatalf("auto segment must show its verdict: %d", status)
	}
	if !strings.Contains(body, "view=bar") || !strings.Contains(body, "view=table") {
		t.Fatal("view segment links missing")
	}
}

func TestExplorerSaveAsPanel(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	// no dashboards yet: the save row offers creating one instead
	status, body := cl.get("/metrics-explorer")
	if status != http.StatusOK || !strings.Contains(body, "Create a dashboard") {
		t.Fatalf("save row without dashboards wrong: %d", status)
	}
	_, dashBody := cl.get("/dashboards")
	token := csrfRe.FindStringSubmatch(dashBody)[1]
	cl.postForm("/dashboards", url.Values{"csrf_token": {token}, "name": {"kept views"}})

	// with a dashboard, saving works and redirects into it
	resp := cl.postForm("/metrics-explorer/save-panel", url.Values{
		"csrf_token":   {token},
		"query":        {"SELECT timestamp, value FROM metrics WHERE name = 'queue_depth' ORDER BY timestamp"},
		"chart_type":   {"auto"},
		"title":        {"queue depth"},
		"dashboard_id": {"1"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/dashboards/1" {
		t.Fatalf("save panel = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	status, frag := cl.get("/dashboards/1/panels/1")
	if status != http.StatusOK || !strings.Contains(frag, `"type":"line"`) {
		t.Fatalf("saved panel fragment: %d", status)
	}

	// mixed-signal panel queries are rejected
	resp = cl.postForm("/metrics-explorer/save-panel", url.Values{
		"csrf_token":   {token},
		"query":        {"SELECT * FROM logs l JOIN metrics m ON l.service = m.service"},
		"chart_type":   {"auto"},
		"title":        {"nope"},
		"dashboard_id": {"1"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mixed-signal save = %d, want 400", resp.StatusCode)
	}
}

var snippetRe = regexp.MustCompile(`data-snippet="([^"]+)"`)

// Snippets are generic patterns with screaming placeholders: running one
// unedited gets a guided error, and with placeholders substituted every
// snippet must validate and execute.
func TestExplorerSnippets(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	_, body := cl.get("/metrics-explorer")
	matches := snippetRe.FindAllStringSubmatch(body, -1)
	if len(matches) != 6 {
		t.Fatalf("expected 6 snippets, got %d", len(matches))
	}

	substitutions := strings.NewReplacer(
		"REPLACE_WITH_YOUR_COUNTER_NAME", "orders_total",
		"REPLACE_WITH_YOUR_GAUGE_NAME", "queue_depth",
		"REPLACE_WITH_BASE_NAME", "req_duration_seconds",
		"REPLACE_WITH_SERVICE_A", "shop",
		"REPLACE_WITH_SERVICE_B", "warehouse",
	)
	sawPlaceholder := false
	for i, m := range matches {
		raw := html.UnescapeString(m[1])

		if strings.Contains(raw, "REPLACE_WITH_") {
			sawPlaceholder = true
			// unedited: guided error, not silent zero rows
			status, page := cl.get("/metrics-explorer?q=" + url.QueryEscape(raw))
			if status != http.StatusBadRequest || !strings.Contains(page, "Replace REPLACE_WITH_") {
				t.Errorf("snippet %d unedited: want guided placeholder error, got %d", i, status)
			}
		}

		// substituted: must validate and run
		q := substitutions.Replace(raw)
		status, page := cl.get("/metrics-explorer?q=" + url.QueryEscape(q))
		if status != http.StatusOK || strings.Contains(page, "alert-error") {
			t.Errorf("snippet %d failed to run after substitution: %d\n%s", i, status, q)
		}
	}
	if !sawPlaceholder {
		t.Error("no snippet carries a placeholder; the guided-error path is untested")
	}

	// saving a placeholder query as a panel is caught too
	_, dashBody := cl.get("/dashboards")
	token := csrfRe.FindStringSubmatch(dashBody)[1]
	cl.postForm("/dashboards", url.Values{"csrf_token": {token}, "name": {"d"}})
	resp := cl.postForm("/metrics-explorer/save-panel", url.Values{
		"csrf_token":   {token},
		"query":        {"SELECT value FROM metrics WHERE name = 'REPLACE_WITH_YOUR_GAUGE_NAME'"},
		"chart_type":   {"auto"},
		"title":        {"nope"},
		"dashboard_id": {"1"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("placeholder panel save = %d, want 400", resp.StatusCode)
	}
}

// Logs snippets: same contract as the explorer ones - placeholders get the
// guided inline error, substituted versions validate and run.
func TestLogsSnippets(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 4) // logs_test helper: services payment-svc/auth-svc on host-1

	_, body := cl.get("/logs")
	matches := snippetRe.FindAllStringSubmatch(body, -1)
	if len(matches) != 4 {
		t.Fatalf("expected 4 logs snippets, got %d", len(matches))
	}

	substitutions := strings.NewReplacer(
		"REPLACE_WITH_FIELD_NAME", "status",
		"REPLACE_WITH_HOST_NAME", "host-1",
		"REPLACE_WITH_SERVICE_NAME", "auth-svc",
	)
	for i, m := range matches {
		raw := html.UnescapeString(m[1])

		if strings.Contains(raw, "REPLACE_WITH_") {
			// unedited: inline guided error, no silent empty view
			_, page := cl.get("/logs?q=" + url.QueryEscape(raw))
			if !strings.Contains(page, "Replace REPLACE_WITH_") {
				t.Errorf("logs snippet %d unedited: guided error missing", i)
			}
			// export path blocks it outright
			if status, _ := cl.get("/logs/export?q=" + url.QueryEscape(raw)); status != http.StatusBadRequest {
				t.Errorf("logs snippet %d unedited export = %d, want 400", i, status)
			}
		}

		q := substitutions.Replace(raw)
		status, page := cl.get("/logs?q=" + url.QueryEscape(q))
		if status != http.StatusOK || strings.Contains(page, "Replace REPLACE_WITH_") {
			t.Errorf("logs snippet %d failed after substitution: %d\n%s", i, status, q)
		}
	}

	// the host+service snippet substituted actually filters to the seeded rows
	q := "SELECT * FROM logs WHERE host = 'host-1' AND service = 'auth-svc' ORDER BY timestamp DESC LIMIT 100"
	_, page := cl.get("/logs?q=" + url.QueryEscape(q))
	if !strings.Contains(page, "message 1") || strings.Contains(page, "message 0") {
		t.Error("host+service query did not filter to the seeded auth-svc rows")
	}
}

// The DSL compile endpoint: DSL in, runnable SQL out; errors ride the 200 as
// hints. Pivots get real service names from the catalog.
func TestDSLCompileEndpoint(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	compile := func(dsl string) (int, string) {
		return cl.get("/metrics-explorer/compile?dsl=" + url.QueryEscape(dsl))
	}

	status, body := compile("avg(queue_depth) per 10m last 6h")
	if status != http.StatusOK || !strings.Contains(body, `"sql"`) || !strings.Contains(body, "AVG(value)") {
		t.Fatalf("compile: %d %s", status, body)
	}
	// the parsed structure feeds the query builder (its only grammar source)
	for _, want := range []string{`"verb":"avg"`, `"metric":"queue_depth"`, `"perMs":600000`, `"lastMs":21600000`} {
		if !strings.Contains(body, want) {
			t.Errorf("parsed structure missing %s: %s", want, body)
		}
	}

	// the compiled SQL must run through the explorer end to end
	var res struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	status, page := cl.get("/metrics-explorer?q=" + url.QueryEscape(res.SQL))
	if status != http.StatusOK || strings.Contains(page, "alert-error") {
		t.Fatalf("compiled SQL failed in the explorer: %d", status)
	}

	// pivot pulls real service names from the catalog
	_, body = compile("avg(queue_depth) per 10m by service")
	if !strings.Contains(body, `service = 'shop'`) {
		t.Fatalf("pivot missing catalog service: %s", body)
	}

	// filters resolve: column vs labels
	_, body = compile("count(orders_total) service=shop status=ok per 1h")
	if !strings.Contains(body, "json_extract(labels, '$.status')") || !strings.Contains(body, `service = 'shop'`) {
		t.Fatalf("filter resolution wrong: %s", body)
	}

	// parse errors are hints, not failures
	status, body = compile("averg(queue_depth)")
	if status != http.StatusOK || !strings.Contains(body, "unknown verb") {
		t.Fatalf("parse error handling: %d %s", status, body)
	}

	// the page carries the DSL input, the builder, the names, and preserves
	// dsl in view links
	_, page = cl.get("/metrics-explorer?dsl=" + url.QueryEscape("avg(queue_depth)") + "&q=" + url.QueryEscape("SELECT AVG(value) AS avg_value FROM metrics WHERE name = 'queue_depth'"))
	for _, want := range []string{
		`id="dsl-input"`, `id="metric-names"`, "queue_depth", "dsl=avg%28queue_depth%29",
		`id="dsl-builder"`, `id="dsl-mode-builder"`, `id="dsl-mode-text"`, `id="metric-names-dl"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("explorer page missing %q", want)
		}
	}

	// dsl is authoritative: a stale submitted q loses to the recompiled dsl,
	// closing the client debounce race
	_, page = cl.get("/metrics-explorer?dsl=" + url.QueryEscape("avg(queue_depth) last 1h") +
		"&q=" + url.QueryEscape("SELECT value FROM metrics WHERE name = 'queue_depth' LIMIT 3"))
	if !strings.Contains(page, "AVG(value)") || strings.Contains(page, "LIMIT 3") {
		t.Error("dsl did not outrank the stale q")
	}
}

// The filter-options endpoint offers what exists: keys for a metric
// (service/host + its label keys), then values for a chosen key.
func TestFilterOptions(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	status, body := cl.get("/metrics-explorer/filter-options?metric=orders_total")
	if status != http.StatusOK {
		t.Fatalf("keys: %d", status)
	}
	for _, want := range []string{`"service"`, `"host"`, `"status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("keys missing %s: %s", want, body)
		}
	}

	_, body = cl.get("/metrics-explorer/filter-options?metric=orders_total&key=status")
	if !strings.Contains(body, `"ok"`) {
		t.Errorf("label values missing: %s", body)
	}
	_, body = cl.get("/metrics-explorer/filter-options?metric=orders_total&key=service")
	if !strings.Contains(body, `"shop"`) {
		t.Errorf("service values missing: %s", body)
	}
	// hostile key shapes return empty options, never an error
	status, body = cl.get("/metrics-explorer/filter-options?metric=orders_total&key=" + url.QueryEscape(`x"];drop`))
	if status != http.StatusOK || strings.Contains(body, "drop") {
		t.Errorf("bad key handling: %d %s", status, body)
	}
}

// Three snippets carry authored DSL equivalents (never decompiled); chips
// teach the DSL by filling both layers.
func TestSnippetDSLEquivalents(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	_, body := cl.get("/metrics-explorer")
	withDSL := regexp.MustCompile(`data-dsl-snippet="[^"]+"`).FindAllString(body, -1)
	if len(withDSL) != 3 {
		t.Fatalf("expected 3 snippets with DSL equivalents, got %d", len(withDSL))
	}
	// and the formatter is pinned in the import map
	if !strings.Contains(body, "sql-formatter@15.8.2") {
		t.Error("sql-formatter missing from the import map")
	}
}

var explorerChartRe = regexp.MustCompile(`(?s)<script[^>]*id="chart-data-explorer"[^>]*>(.*?)</script>`)

// Counter semantics end to end: raw-value verbs on a counter get an advisory
// hint, and DSL charts cover the whole requested window - zeros for counter
// verbs (no rows = zero events), null gaps for gauge verbs (no samples =
// unknown).
func TestCounterHintAndGapFill(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	now := time.Now().UnixMilli()
	bucket := now / 600000 * 600000
	_, err := cl.pools.Metrics.Exec(`INSERT INTO metrics(timestamp, name, type, value, service, host) VALUES
		(?, 'queue_depth', 1, 7, 'shop', 'h1'),
		(?, 'queue_depth', 1, 9, 'shop', 'h1'),
		(?, 'orders_total', 0, 100, 'shop', 'h1'),
		(?, 'orders_total', 0, 140, 'shop', 'h1')`,
		bucket-1200000, bucket, bucket+1000, bucket+2000)
	if err != nil {
		t.Fatal(err)
	}

	// advisory hint: avg() pointed at a counter
	_, body := cl.get("/metrics-explorer/compile?dsl=" + url.QueryEscape("avg(orders_total) per 10m"))
	if !strings.Contains(body, "is a counter") {
		t.Errorf("counter hint missing: %s", body)
	}
	// no hint for a gauge, none for increase on the counter
	_, body = cl.get("/metrics-explorer/compile?dsl=" + url.QueryEscape("avg(queue_depth) per 10m"))
	if strings.Contains(body, "is a counter") {
		t.Error("gauge got a counter hint")
	}
	_, body = cl.get("/metrics-explorer/compile?dsl=" + url.QueryEscape("increase(orders_total) per 10m"))
	if strings.Contains(body, "is a counter") {
		t.Error("increase() got a counter hint")
	}

	chartOf := func(page string) (labels []int64, data []*float64) {
		t.Helper()
		m := explorerChartRe.FindStringSubmatch(page)
		if m == nil {
			if i := strings.Index(page, "chart-data-explorer"); i >= 0 {
				t.Fatalf("script tag shape unexpected: %q", page[max(0, i-120):min(len(page), i+200)])
			}
			t.Fatalf("no chart payload in page")
		}
		var payload struct {
			LabelsMs []int64 `json:"labelsMs"`
			Datasets []struct {
				Data []*float64 `json:"data"`
			} `json:"datasets"`
		}
		if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Datasets) == 0 {
			t.Fatal("no datasets")
		}
		return payload.LabelsMs, payload.Datasets[0].Data
	}

	// gauge: full 1h grid (7 buckets of 10m), missing buckets are nulls
	_, page := cl.get("/metrics-explorer?dsl=" + url.QueryEscape("avg(queue_depth) per 10m last 1h"))
	labels, data := chartOf(page)
	if len(labels) != 7 || len(data) != 7 {
		t.Fatalf("gauge grid = %d buckets, want 7", len(labels))
	}
	nulls, values := 0, 0
	for _, d := range data {
		if d == nil {
			nulls++
		} else {
			values++
		}
	}
	if values != 2 || nulls != 5 {
		t.Fatalf("gauge fill: %d values, %d nulls (want 2 and 5)", values, nulls)
	}

	// counter: same grid, missing buckets are ZEROS and the data bucket is 40
	_, page = cl.get("/metrics-explorer?dsl=" + url.QueryEscape("increase(orders_total) per 10m last 1h"))
	labels, data = chartOf(page)
	if len(labels) != 7 {
		t.Fatalf("counter grid = %d buckets, want 7", len(labels))
	}
	zeros, sum := 0, 0.0
	for _, d := range data {
		if d == nil {
			t.Fatal("counter fill must never be null")
		}
		if *d == 0 {
			zeros++
		}
		sum += *d
	}
	if zeros != 6 || sum != 40 {
		t.Fatalf("counter fill: %d zeros, sum %v (want 6 and 40)", zeros, sum)
	}
}

var dashboardURLRe = regexp.MustCompile(`/dashboards/(\d+)`)

func TestDashboardsFlow(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl)

	status, body := cl.get("/dashboards")
	if status != http.StatusOK || !strings.Contains(body, "No dashboards yet") {
		t.Fatalf("dashboards list: %d", status)
	}
	token := csrfRe.FindStringSubmatch(body)[1]

	// create redirects into the new dashboard
	resp := cl.postForm("/dashboards", url.Values{
		"csrf_token": {token},
		"name":       {"shop overview"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create dashboard = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !dashboardURLRe.MatchString(loc) {
		t.Fatalf("create redirect = %q", loc)
	}

	status, body = cl.get(loc)
	if status != http.StatusOK || !strings.Contains(body, "shop overview") || !strings.Contains(body, "Add panel") {
		t.Fatalf("dashboard page: %d", status)
	}

	// add three panels: stat, line (time x), bar (category x)
	for _, p := range []url.Values{
		{"title": {"total points"}, "query": {"SELECT COUNT(*) FROM metrics"}, "chart_type": {""}, "width": {"1"}},
		{"title": {"queue depth"}, "query": {"SELECT timestamp, value FROM metrics WHERE name = 'queue_depth' ORDER BY timestamp"}, "chart_type": {""}, "width": {"2"}},
		{"title": {"by service"}, "query": {"SELECT service, COUNT(*) AS points FROM metrics GROUP BY service"}, "chart_type": {""}, "width": {"3"}},
	} {
		p.Set("csrf_token", token)
		if resp := cl.postForm(loc+"/panels", p); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("add panel %q = %d", p.Get("title"), resp.StatusCode)
		}
	}

	// invalid panel query rejected
	bad := url.Values{"csrf_token": {token}, "title": {"nope"}, "query": {"DELETE FROM metrics"}, "width": {"1"}}
	if resp := cl.postForm(loc+"/panels", bad); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-SELECT panel = %d, want 400", resp.StatusCode)
	}

	// panel fragments: stat renders the number, line embeds time labels,
	// bar embeds category labels
	status, frag := cl.get(loc + "/panels/1")
	if status != http.StatusOK || !strings.Contains(frag, ">3<") {
		t.Fatalf("stat fragment: %d, %.200s", status, frag)
	}
	status, frag = cl.get(loc + "/panels/2")
	if status != http.StatusOK || !strings.Contains(frag, `"type":"line"`) || !strings.Contains(frag, "labelsMs") {
		t.Fatalf("line fragment: %d, %.300s", status, frag)
	}
	status, frag = cl.get(loc + "/panels/3")
	if status != http.StatusOK || !strings.Contains(frag, `"type":"bar"`) || !strings.Contains(frag, "shop") {
		t.Fatalf("bar fragment: %d, %.300s", status, frag)
	}

	// edit form + cancel-card round trip
	status, frag = cl.get(loc + "/panels/1?edit=1")
	if status != http.StatusOK || !strings.Contains(frag, "sql-editor-host") {
		t.Fatalf("edit form: %d", status)
	}
	status, frag = cl.get(loc + "/panels/1?card=1")
	if status != http.StatusOK || !strings.Contains(frag, "total points") {
		t.Fatalf("card restore: %d", status)
	}

	// delete a panel (empty 200, card swaps itself away)
	status, frag = cl.htmx(http.MethodDelete, loc+"/panels/1", token)
	if status != http.StatusOK || strings.TrimSpace(frag) != "" {
		t.Fatalf("delete panel: %d, body %q", status, frag)
	}

	// delete dashboard cascades panels
	status, _ = cl.htmx(http.MethodDelete, loc, token)
	if status != http.StatusSeeOther && status != http.StatusOK {
		t.Fatalf("delete dashboard: %d", status)
	}
	var n int
	if err := cl.pools.Meta.QueryRow("SELECT COUNT(*) FROM dashboard_panels").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("panels survived dashboard delete: %d", n)
	}
}
