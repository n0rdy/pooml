package ui_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWarRoomDefaultPage(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 6) // recent rows, inside the default 1h window

	status, body := cl.get("/war-room")
	if status != http.StatusOK {
		t.Fatalf("GET /war-room = %d", status)
	}
	for _, want := range []string{
		"War Room",
		"war-window-label",
		"data-span-ms",     // presets
		"war-follow",       // follow-now toggle
		"chart-data-wmini", // the auto volume/error mini chart
		"add a metric row", // empty metric rows offer authoring
		`name="fts"`,       // logs-page controls, verbatim
		"level &gt;= 3",    // default query visible in the hidden textarea
		"message 3",        // level-4 rows pass the default level >= 3 filter
	} {
		if !strings.Contains(body, want) {
			t.Errorf("war room missing %q", want)
		}
	}
	// info-level rows stay out under the default query
	if strings.Contains(body, "message 2") {
		t.Error("level-2 row shown despite the default level >= 3 filter")
	}
}

func TestWarRoomWindowLocksLogs(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	insert := func(ts int64, raw string) {
		if _, err := cl.pools.LogsWrite.Exec(
			`INSERT INTO logs(timestamp, ingested_at, level, service, host, message, parsed, raw)
			 VALUES (?, ?, 4, 'svc', 'h', ?, NULL, ?)`, ts, ts, raw, raw); err != nil {
			cl.t.Fatal(err)
		}
	}
	insert(1_723_600_100_000, "inside-the-window")
	insert(1_723_700_000_000, "outside-the-window")

	q := url.Values{"from": {"1723600000000"}, "to": {"1723600200000"}}
	status, body := cl.get("/war-room?" + q.Encode())
	if status != http.StatusOK {
		t.Fatalf("windowed war room = %d", status)
	}
	if !strings.Contains(body, "inside-the-window") {
		t.Error("row inside the window missing")
	}
	if strings.Contains(body, "outside-the-window") {
		t.Error("row outside the window leaked in")
	}
	// the URL's q stays window-free: BETWEEN is injected at execution only
	if strings.Contains(body, "BETWEEN 1723600000000") {
		t.Error("window predicate leaked into the visible query")
	}
}

func TestWarRoomMetricRow(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedMetrics(cl) // queue_depth points at 1723600000000..1723600030000

	q := url.Values{
		"from": {"1723599900000"},
		"to":   {"1723600300000"},
		"dsl":  {"avg(queue_depth) per 1m last 24h"},
	}
	status, body := cl.get("/war-room?" + q.Encode())
	if status != http.StatusOK {
		t.Fatalf("war room with dsl = %d", status)
	}
	if !strings.Contains(body, "chart-data-w0") {
		t.Errorf("metric row chart data missing")
	}
	// bad DSL: row-scoped error, page still renders
	q.Set("dsl", "nonsense(")
	status, body = cl.get("/war-room?" + q.Encode())
	if status != http.StatusOK || !strings.Contains(body, "start with a verb") {
		t.Errorf("bad dsl: status %d, row error present: %v", status, strings.Contains(body, "start with a verb"))
	}
}

func TestWarRoomPanesFragment(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 2)

	status, body := cl.get("/war-room?frag=panes")
	if status != http.StatusOK {
		t.Fatalf("frag=panes = %d", status)
	}
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Error("panes fragment must swap out-of-band")
	}
	for _, id := range []string{"war-window-label", "war-chart-0", "war-mini", "war-logs"} {
		if !strings.Contains(body, fmt.Sprintf(`id="%s"`, id)) {
			t.Errorf("panes fragment missing region %q", id)
		}
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "war-form") {
		t.Error("panes fragment leaked the full page")
	}
}

// The War Room's entry point is the home page (a peer of Triage), NOT the
// nav bar - it's an incident door, not a daily destination.
func TestWarRoomEntryPoint(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	if _, body := cl.get("/"); !strings.Contains(body, `href="/war-room"`) {
		t.Error("war room entry missing from the home page")
	}
	if _, body := cl.get("/logs"); strings.Contains(body, `href="/war-room"`) {
		t.Error("war room leaked into the nav bar")
	}
}
