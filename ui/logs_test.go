package ui_test

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seedLogs inserts n rows with ascending timestamps/ids: service cycles
// between payment-svc and auth-svc, level between info and error.
func seedLogs(cl *client, n int) {
	cl.t.Helper()
	base := time.Now().UnixMilli() - int64(n)*1000
	for i := range n {
		service, level := "payment-svc", 2
		if i%2 == 1 {
			service, level = "auth-svc", 4
		}
		raw := fmt.Sprintf("seeded line %d needle%d", i, i)
		if _, err := cl.pools.LogsWrite.Exec(
			`INSERT INTO logs(timestamp, ingested_at, level, service, host, message, parsed, raw)
			 VALUES (?, ?, ?, ?, 'host-1', ?, NULL, ?)`,
			base+int64(i)*1000, base+int64(i)*1000, level, service, fmt.Sprintf("message %d", i), raw); err != nil {
			cl.t.Fatal(err)
		}
	}
}

func (cl *client) getWith(path string, headers map[string]string) *http.Response {
	cl.t.Helper()
	req, err := http.NewRequest(http.MethodGet, cl.srv.URL+path, nil)
	if err != nil {
		cl.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cl.c.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	return resp
}

func TestLogsPageFull(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 4)

	status, body := cl.get("/logs")
	if status != http.StatusOK {
		t.Fatalf("GET /logs = %d", status)
	}
	for _, want := range []string{
		`data-q="SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100"`,
		"importmap",
		"esm.sh/*codemirror@6.0.2",
		`import { EditorView, basicSetup } from "codemirror"`,
		"message 3",             // seeded rows rendered
		"payment-svc",           // badge
		"reaching further back", // sentinel: history probe not yet run
	} {
		if !strings.Contains(body, want) {
			t.Errorf("full page missing %q", want)
		}
	}
}

func TestLogsFragmentAndShapes(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 4)
	hx := map[string]string{"HX-Request": "true"}

	// log-viewer fragment: no page chrome, rows ascending (newest last)
	resp := cl.getWith("/logs?q="+url.QueryEscape("SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100"), hx)
	body := readBody(t, resp)
	if strings.Contains(body, "sql-editor") {
		t.Error("fragment contains full-page chrome")
	}
	if strings.Index(body, "message 0") > strings.Index(body, "message 3") {
		t.Error("rows not ascending (newest should be at the bottom)")
	}

	// aggregation renders the generic table
	resp = cl.getWith("/logs?q="+url.QueryEscape("SELECT service, count(*) AS cnt FROM logs GROUP BY service ORDER BY cnt DESC"), hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "cnt") || strings.Contains(body, "log-row") {
		t.Errorf("aggregation should render generic table, got: %.200s", body)
	}

	// invalid SQL yields the friendly error alert
	resp = cl.getWith("/logs?q=DROP+TABLE+logs", hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "alert-error") || !strings.Contains(body, "SELECT") {
		t.Errorf("error fragment: %.200s", body)
	}

	// positioned parse errors are humanized and carry a sqlError trigger for
	// the editor to underline
	resp = cl.getWith("/logs?q="+url.QueryEscape("SELECT * FROM logs 100"), hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "Line 1, col") || !strings.Contains(body, "should end here") {
		t.Errorf("humanized error missing: %.200s", body)
	}
	if trig := resp.Header.Get("HX-Trigger"); !strings.Contains(trig, "sqlError") || !strings.Contains(trig, `"line":1`) {
		t.Errorf("sqlError trigger = %q", trig)
	}
}

func TestLogsQuickFilter(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 4)
	hx := map[string]string{"HX-Request": "true"}

	q := url.QueryEscape("SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100")
	resp := cl.getWith("/logs?q="+q+"&service=auth-svc", hx)
	body := readBody(t, resp)

	trigger := resp.Header.Get("HX-Trigger")
	if !strings.Contains(trigger, "setQuery") || !strings.Contains(trigger, "service = 'auth-svc'") {
		t.Errorf("HX-Trigger = %q", trigger)
	}
	push := resp.Header.Get("HX-Push-Url")
	if !strings.Contains(push, url.QueryEscape("service = 'auth-svc'")) {
		t.Errorf("HX-Push-Url = %q", push)
	}
	if strings.Contains(body, "payment-svc</button>") {
		t.Error("filtered results still contain the other service")
	}

	// clicking the same badge again over the merged q must not stack ANDs
	merged := regexp.MustCompile(`"q":"([^"]+)"`).FindStringSubmatch(trigger)
	if merged == nil {
		t.Fatal("no q in HX-Trigger payload")
	}
	resp = cl.getWith("/logs?q="+url.QueryEscape(merged[1])+"&service=auth-svc", hx)
	readBody(t, resp)
	again := resp.Header.Get("HX-Trigger")
	if strings.Count(again, "service = 'auth-svc'") != 1 {
		t.Errorf("repeated filter stacked conditions: %q", again)
	}

	// filter on an aggregation drills in to raw logs
	aggQ := url.QueryEscape("SELECT service, count(*) FROM logs GROUP BY service")
	resp = cl.getWith("/logs?q="+aggQ+"&service=auth-svc", hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "log-row") {
		t.Errorf("drill-in should render the log viewer, got: %.200s", body)
	}
}

func TestLogsFTSAndPagination(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 70)
	hx := map[string]string{"HX-Request": "true"}

	// FTS narrows to the one matching row
	resp := cl.getWith("/logs?fts=needle42", hx)
	body := readBody(t, resp)
	if !strings.Contains(body, "message 42") || strings.Contains(body, "message 41") {
		t.Errorf("fts filtering broken: has 42: %v", strings.Contains(body, "message 42"))
	}

	// initial view always offers the sentinel; the probe that finds nothing
	// older resolves to the beginning-of-history marker
	resp = cl.getWith("/logs", hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "reaching further back") {
		t.Error("sentinel missing on initial view")
	}
	// following the sentinel of the full view (all 70 rows shown) finds
	// nothing older and renders the beginning marker
	resp = cl.getWith(sentinelURL(t, body), hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "beginning of history") {
		t.Errorf("empty probe should render the beginning marker, got: %.200s", body)
	}

	// LIMIT 20 shows rows 51..70; the sentinel's composite cursor pages in
	// the 50 rows before them
	q := url.QueryEscape("SELECT * FROM logs ORDER BY timestamp DESC LIMIT 20")
	resp = cl.getWith("/logs?q="+q, hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "reaching further back") {
		t.Fatalf("sentinel missing with more history available")
	}
	if !strings.Contains(body, "message 50") || strings.Contains(body, "message 49") {
		t.Fatalf("LIMIT 20 window wrong")
	}
	resp = cl.getWith(sentinelURL(t, body), hx)
	body = readBody(t, resp)
	if !strings.Contains(body, "message 30") || strings.Contains(body, "message 50") {
		t.Errorf("pagination window wrong")
	}
	if !strings.Contains(body, "reaching further back") {
		t.Error("non-empty page keeps its sentinel")
	}
}

func TestLogsDetailAndExport(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 3)

	// detail fragment
	resp := cl.getWith("/logs/1", map[string]string{"HX-Request": "true"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "seeded line 0") {
		t.Errorf("detail = %d: %.200s", resp.StatusCode, body)
	}
	if resp := cl.getWith("/logs/999", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing id = %d, want 404", resp.StatusCode)
	}

	// csv export
	resp = cl.getWith("/logs/export?q="+url.QueryEscape("SELECT service, message FROM logs ORDER BY id"), nil)
	body = readBody(t, resp)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Errorf("csv content type = %q", ct)
	}
	if !strings.HasPrefix(body, "service,message\n") || !strings.Contains(body, "payment-svc,message 0") {
		t.Errorf("csv body: %.200s", body)
	}

	// json export parses and carries the columns
	resp = cl.getWith("/logs/export?format=json&q="+url.QueryEscape("SELECT service, message FROM logs ORDER BY id"), nil)
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("json export: %v", err)
	}
	resp.Body.Close()
	if len(rows) != 3 || rows[0]["service"] != "payment-svc" {
		t.Errorf("json rows = %+v", rows)
	}
}

var sentinelRe = regexp.MustCompile(`hx-get="([^"]*before_ts[^"]*)"`)

// sentinelURL extracts the pagination cursor URL the server rendered.
func sentinelURL(t *testing.T, body string) string {
	t.Helper()
	m := sentinelRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no pagination sentinel in body")
	}
	return html.UnescapeString(m[1])
}

// Back-button restores must get the full page: htmx's DOM-snapshot cache is
// disabled (CodeMirror state restores wrong), so history misses re-fetch.
func TestLogsHistoryRestoreGetsFullPage(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 2)

	resp := cl.getWith("/logs", map[string]string{
		"HX-Request":                 "true",
		"HX-History-Restore-Request": "true",
	})
	body := readBody(t, resp)
	if !strings.Contains(body, "sql-editor") {
		t.Error("history restore should render the full page, got a fragment")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
