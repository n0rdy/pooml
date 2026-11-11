package ui_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHomePageAndTriage(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	status, body := cl.get("/")
	if status != http.StatusOK {
		t.Fatalf("home = %d", status)
	}
	for _, want := range []string{
		"Triage",
		"level+%3E%3D+4", // triage query in the link, url-encoded
		"live=1",
		`hx-get="/home/volume"`,
		`hx-get="/home/errors"`,
		`hx-get="/home/services"`,
		"every 30s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home missing %q", want)
		}
	}
}

func TestHomeFragments(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	hx := map[string]string{"HX-Request": "true"}

	// empty states first
	resp := cl.getWith("/home/errors", hx)
	if body := readBody(t, resp); !strings.Contains(body, "Zero errors") {
		t.Errorf("errors empty state: %.200s", body)
	}
	resp = cl.getWith("/home/volume", hx)
	if body := readBody(t, resp); !strings.Contains(body, "Not a single log") {
		t.Errorf("volume empty state: %.200s", body)
	}

	seedLogs(cl, 6) // recent: 3 payment-svc info, 3 auth-svc error

	resp = cl.getWith("/home/volume", hx)
	body := readBody(t, resp)
	if !strings.Contains(body, "6 logs") || !strings.Contains(body, "bg-primary") {
		t.Errorf("volume fragment: %.300s", body)
	}

	resp = cl.getWith("/home/errors", hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "auth-svc") || strings.Contains(body, "payment-svc") {
		t.Errorf("errors fragment should list only the erroring service: %.300s", body)
	}
	if !strings.Contains(body, ">3</span>") {
		t.Errorf("errors count missing: %.300s", body)
	}
	// drill-down link carries the 24h error query
	if !strings.Contains(body, "level+%3E%3D+4") {
		t.Error("error drill-down link missing level filter")
	}

	resp = cl.getWith("/home/services", hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "payment-svc") || !strings.Contains(body, "auth-svc") {
		t.Errorf("services fragment: %.300s", body)
	}
	if !strings.Contains(body, "data-reltime") {
		t.Error("services fragment missing relative timestamps")
	}
}

func TestHomeFragmentsRequireAuth(t *testing.T) {
	cl := newClient(t)
	if resp := cl.getWith("/home/errors", nil); resp.StatusCode != http.StatusFound {
		t.Errorf("unauthenticated fragment = %d, want 302", resp.StatusCode)
	}
}

func TestHomeMetricsServicesFragment(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	hx := map[string]string{"HX-Request": "true"}

	resp := cl.getWith("/home/metrics-services", hx)
	if body := readBody(t, resp); !strings.Contains(body, "No services reporting metrics") {
		t.Errorf("metrics-services empty state: %.200s", body)
	}

	// recent point shows up; a stale one (beyond the 7-day bound) does not
	now := time.Now().UnixMilli()
	if _, err := cl.pools.Metrics.Exec(`INSERT INTO metrics(timestamp, name, type, value, service, host, labels) VALUES
		(?, 'queue_depth', 1, 7, 'shop', 'h1', NULL),
		(?, 'queue_depth', 1, 9, 'ancient-svc', 'h1', NULL)`,
		now-60_000, now-8*24*60*60*1000); err != nil {
		t.Fatal(err)
	}
	resp = cl.getWith("/home/metrics-services", hx)
	body := readBody(t, resp)
	if !strings.Contains(body, "shop") || strings.Contains(body, "ancient-svc") {
		t.Errorf("metrics-services fragment: %.300s", body)
	}
	// link lands on the explorer scoped to that service
	if !strings.Contains(body, "/metrics-explorer?") || !strings.Contains(body, "shop") {
		t.Error("metrics-services drill-down link missing")
	}

	// the home page itself wires the new card
	if _, page := cl.get("/"); !strings.Contains(page, `hx-get="/home/metrics-services"`) {
		t.Error("home page missing the metrics-services card")
	}
}

// Broken alerts and failing backups get a card on Home, and the card does not
// exist otherwise.
func TestHomeAttentionFragment(t *testing.T) {
	cl := newClient(t)
	if resp := cl.login(testSecret, cl.csrfToken()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	if _, page := cl.get("/"); !strings.Contains(page, `hx-get="/home/attention"`) {
		t.Error("home page missing the attention fragment")
	}

	insert := func(name string, enabled int, lastError any) {
		t.Helper()
		if _, err := cl.pools.Meta.Exec(
			`INSERT INTO alerts (name, query, check_interval_ms, cooldown_ms, target, enabled, last_error, last_error_at, created_at)
			 VALUES (?, 'SELECT 1 FROM logs', 30000, 0, '{"type":"pushover"}', ?, ?, 1, 1)`, name, enabled, lastError); err != nil {
			t.Fatal(err)
		}
	}
	insert("healthy", 1, nil)
	insert("disabled and broken", 0, "query failed: whatever")
	if status, body := cl.get("/home/attention"); status != http.StatusOK || strings.TrimSpace(body) != "" {
		t.Fatalf("nothing broken: status=%d body=%q, want an empty fragment", status, body)
	}

	insert(`blind <b>one</b>`, 1, `table "sqlite_master" is not allowed`)
	insert("mute one", 1, "notification failed: campfire rejected the message: HTTP 500")
	_, body := cl.get("/home/attention")
	for _, want := range []string{"Alerts flying blind", "blind &lt;b&gt;one&lt;/b&gt;", "sqlite_master", "mute one", "notification failed", `href="/alerts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
	for _, not := range []string{"healthy", "disabled and broken", "<b>one</b>", "Backups failing"} {
		if strings.Contains(body, not) {
			t.Errorf("fragment must not contain %q", not)
		}
	}

	// backups: shown only while switched on AND the last run failed
	if _, err := cl.pools.Meta.Exec("DELETE FROM alerts"); err != nil {
		t.Fatal(err)
	}
	setting := func(key, val string) {
		t.Helper()
		if _, err := cl.pools.Meta.Exec(
			`INSERT INTO settings (key, value, is_encrypted, updated_at) VALUES (?, ?, 0, 1)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, val); err != nil {
			t.Fatal(err)
		}
	}
	setting("backup.last_result", "upload 2026/logs.db: <AccessDenied>")
	setting("backup.last_run_at", "1700000000000")
	setting("backup.enabled", "false")
	if _, body := cl.get("/home/attention"); strings.TrimSpace(body) != "" {
		t.Fatalf("backups switched off: want an empty fragment, got %q", body)
	}
	setting("backup.enabled", "true")
	_, body = cl.get("/home/attention")
	for _, want := range []string{"Backups failing", "&lt;AccessDenied&gt;", `href="/settings"`, `data-reltime="1700000000000"`} {
		if !strings.Contains(body, want) {
			t.Errorf("backup failure fragment missing %q", want)
		}
	}
	if strings.Contains(body, "Alerts flying blind") {
		t.Error("alerts heading shown with no broken alerts")
	}
	setting("backup.last_result", "ok")
	if _, body := cl.get("/home/attention"); strings.TrimSpace(body) != "" {
		t.Fatalf("healthy backups: want an empty fragment, got %q", body)
	}
}
