package ui_test

import (
	"net/http"
	"strings"
	"testing"
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
