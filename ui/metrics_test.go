package ui_test

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
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
