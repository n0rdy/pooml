package ui_test

import (
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

	// default catalog query
	status, body := cl.get("/metrics-explorer")
	if status != http.StatusOK {
		t.Fatalf("explorer = %d", status)
	}
	for _, want := range []string{"queue_depth", "orders_total", "sql-editor-host"} {
		if !strings.Contains(body, want) {
			t.Errorf("explorer missing %q", want)
		}
	}

	// custom query incl. metrics filter
	status, body = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT value FROM metrics WHERE name = 'queue_depth' ORDER BY timestamp"))
	if status != http.StatusOK || !strings.Contains(body, ">7<") || !strings.Contains(body, ">9<") {
		t.Fatalf("custom query: %d", status)
	}

	// cross-DB join validates and executes (0 rows is fine)
	status, _ = cl.get("/metrics-explorer?q=" + url.QueryEscape("SELECT l.raw, m.value FROM logs l JOIN metrics m ON l.service = m.service LIMIT 10"))
	if status != http.StatusOK {
		t.Fatalf("cross-DB join: %d", status)
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
