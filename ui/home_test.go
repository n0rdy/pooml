package ui_test

import (
	"errors"
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

// The attention card lists what has quietly stopped working, under a heading
// that says which kind, and does not exist while everything works.
func TestHomeAttentionFragment(t *testing.T) {
	cl := newClient(t)
	if resp := cl.login(testSecret, cl.csrfToken()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	if _, page := cl.get("/"); !strings.Contains(page, `hx-get="/home/attention"`) {
		t.Error("home page missing the attention fragment")
	}
	fragment := func() string {
		t.Helper()
		status, body := cl.get("/home/attention")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		return body
	}
	expect := func(body string, want, not []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("fragment missing %q", w)
			}
		}
		for _, n := range not {
			if strings.Contains(body, n) {
				t.Errorf("fragment must not contain %q", n)
			}
		}
	}
	insert := func(name, query string, enabled, failures int, lastError any) {
		t.Helper()
		if _, err := cl.pools.Meta.Exec(
			`INSERT INTO alerts (name, query, check_interval_ms, cooldown_ms, target, enabled, failure_count, last_error, last_error_at, created_at)
			 VALUES (?, ?, 30000, 0, '{"type":"pushover"}', ?, ?, ?, 1, 1)`, name, query, enabled, failures, lastError); err != nil {
			t.Fatal(err)
		}
	}
	const good = "SELECT 1 FROM logs"

	// healthy, disabled, and a single slow check: none of these is news
	insert("healthy", good, 1, 0, nil)
	insert("disabled and broken", good, 0, 5, "query failed: whatever")
	insert("one slow check", good, 1, 1, "query failed: interrupted")
	if body := fragment(); strings.TrimSpace(body) != "" {
		t.Fatalf("nothing worth showing: want an empty fragment, got %q", body)
	}

	insert(`blind <b>one</b>`, good, 1, 2, "query failed: interrupted")
	insert("rejected query", "SELECT * FROM sqlite_master", 1, 1, `table "sqlite_master" is not allowed`)
	insert("mute one", good, 1, 0, "notification failed: campfire rejected the message: HTTP 500")
	body := fragment()
	expect(body,
		[]string{"Alerts flying blind", "blind &lt;b&gt;one&lt;/b&gt;", "rejected query", "sqlite_master", "Alerts that can&#39;t deliver", "mute one", `href="/alerts"`},
		[]string{"healthy", "disabled and broken", "one slow check", "<b>one</b>", "Backups failing", "Not being stored"})
	// each alert under its own heading, not both
	blind, mute, _ := strings.Cut(body, "Alerts that can&#39;t deliver")
	if strings.Contains(blind, "mute one") || strings.Contains(mute, "rejected query") {
		t.Error("alerts listed under the wrong heading")
	}

	// a writer that keeps failing; one failed flush moments ago is not shown
	if _, err := cl.pools.Meta.Exec("DELETE FROM alerts"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	cl.pipeline.Health.Failed(errors.New("database or disk is full"), now)
	if body := fragment(); strings.TrimSpace(body) != "" {
		t.Fatalf("a writer failing for moments: want an empty fragment, got %q", body)
	}
	cl.pipeline.Health.OK()
	cl.pipeline.Health.Failed(errors.New("database or disk is full"), now-5*60*1000)
	expect(fragment(), []string{"Not being stored", "logs: database or disk is full", "data-reltime="}, []string{"Alerts flying blind"})
	cl.pipeline.Health.OK()

	// backups: shown only while switched on AND the last run failed
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
	if body := fragment(); strings.TrimSpace(body) != "" {
		t.Fatalf("backups switched off: want an empty fragment, got %q", body)
	}
	setting("backup.enabled", "true")
	expect(fragment(), []string{"Backups failing", "&lt;AccessDenied&gt;", `href="/settings"`, `data-reltime="1700000000000"`}, []string{"Alerts flying blind", "Not being stored"})
	setting("backup.last_result", "ok")
	if body := fragment(); strings.TrimSpace(body) != "" {
		t.Fatalf("healthy backups: want an empty fragment, got %q", body)
	}
}
