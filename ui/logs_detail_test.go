package ui_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLogsDetailCollapsesMultilineValues(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	now := time.Now().UnixMilli()
	jsonLine := `{"@timestamp":"2026-09-11T15:21:13Z","message":"wrapper failed","level":"ERROR","request.id":"req-123",` +
		`"stack_trace":"java.lang.IllegalArgumentException: root cause here\n\tat A.b(A.java:1)\n\tat A.c(A.java:2)\n"}`
	plainTrace := "2026-09-11T15:21:14Z ERROR boom\njava.lang.IllegalStateException: boom\n\tat A.b(A.java:1)"
	for _, row := range []struct {
		message, parsed, raw string
		hasParsed            bool
	}{
		{"wrapper failed", jsonLine, jsonLine, true},
		{plainTrace, "", plainTrace, false},
		{"one line", "", "one line", false},
	} {
		var parsed any
		if row.hasParsed {
			parsed = row.parsed
		}
		if _, err := cl.pools.LogsWrite.Exec(
			`INSERT INTO logs(timestamp, ingested_at, level, service, host, message, parsed, raw)
			 VALUES (?, ?, 4, 'svc', 'host-1', ?, ?, ?)`, now, now, row.message, parsed, row.raw); err != nil {
			t.Fatal(err)
		}
	}

	// JSON log: labeled fields, trace collapsed to its first line, raw folded away
	body := readBody(t, cl.getWith("/logs/1", map[string]string{"HX-Request": "true"}))
	for _, want := range []string{
		"<dt", "request.id", "req-123",
		"java.lang.IllegalArgumentException: root cause here",
		"▸ 2 more lines", "at A.c(A.java:2)",
		"show raw",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("json detail lacks %q:\n%s", want, body)
		}
	}
	// the fields block carries real line breaks; only the folded raw block keeps the JSON escapes
	if fields := body[strings.Index(body, "<dl"):strings.Index(body, "</dl>")]; strings.Contains(fields, `\n\tat`) {
		t.Errorf("fields block leaks escaped newlines:\n%s", fields)
	}

	// plain multi-line raw: first line visible, rest behind the same expander
	body = readBody(t, cl.getWith("/logs/2", map[string]string{"HX-Request": "true"}))
	if !strings.Contains(body, "▸ 2 more lines") || !strings.Contains(body, "2026-09-11T15:21:14Z ERROR boom") {
		t.Errorf("plain detail not collapsed:\n%s", body)
	}
	if strings.Contains(body, "<dt") {
		t.Errorf("plain detail rendered fields:\n%s", body)
	}

	// single line: no expander at all
	body = readBody(t, cl.getWith("/logs/3", map[string]string{"HX-Request": "true"}))
	if strings.Contains(body, "more line") || strings.Contains(body, "<details") {
		t.Errorf("single-line detail has an expander:\n%s", body)
	}
	if resp := cl.getWith("/logs/3", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("standalone page = %d", resp.StatusCode)
	}
}
