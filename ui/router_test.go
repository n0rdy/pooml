package ui_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui"
)

const testSecret = "ui-test-secret-0123456789-0123456789"

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

type client struct {
	t        *testing.T
	c        *http.Client
	srv      *httptest.Server
	pools    *db.Pools
	pipeline *ingestion.Pipeline
}

func newClient(t *testing.T) *client {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	broadcaster := ingestion.NewBroadcaster()
	pipeline := ingestion.NewPipeline(pools.LogsWrite, broadcaster)
	pipeline.Start()
	t.Cleanup(pipeline.Shutdown)

	enc, err := services.NewEncryptionService("ui-test-encryption-key-0123456789012", pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	settings := services.NewSettingsService(pools.Meta, enc)
	router := ui.NewRouter(ui.Deps{
		Sessions:          services.NewSessionsService(),
		Throttling:        services.NewThrottlingService(),
		ApiKeys:           services.NewApiKeysService(pools.Meta),
		Settings:          settings,
		Alerts:            services.NewAlertsService(pools.Meta),
		Notifier:          services.NewNotificationService(settings),
		ScrapeTargets:     services.NewScrapeTargetsService(pools.Meta, enc),
		Dashboards:        services.NewDashboardsService(pools.Meta),
		Backup:            services.NewBackupService(settings, pools.LogsRead, pools.Metrics, pools.Meta),
		Pools:             pools,
		Broadcaster:       broadcaster,
		StreamCtx:         context.Background(),
		AuthSecret:        testSecret,
		Env:               "local",
		TrustProxyHeaders: false,
	})
	srv := httptest.NewServer(router.NewRouter())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, srv: srv, pools: pools, pipeline: pipeline, c: &http.Client{
		Jar: jar,
		// don't follow redirects: assertions need the raw status + Location
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (cl *client) get(path string) (int, string) {
	cl.t.Helper()
	resp, err := cl.c.Get(cl.srv.URL + path)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// csrfToken fetches /login and extracts the form token (cookie lands in jar).
func (cl *client) csrfToken() string {
	cl.t.Helper()
	_, body := cl.get("/login")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		cl.t.Fatal("no csrf token in login page")
	}
	return m[1]
}

// postForm submits like a browser would: nosurf v1.2 rejects unsafe requests
// carrying none of Sec-Fetch-Site/Origin/Referer, and browsers always send at
// least one on form POSTs. A bare Go client must add one explicitly.
func (cl *client) postForm(path string, form url.Values) *http.Response {
	cl.t.Helper()
	req, err := http.NewRequest(http.MethodPost, cl.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		cl.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", cl.srv.URL)
	resp, err := cl.c.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (cl *client) login(secret, token string) *http.Response {
	cl.t.Helper()
	return cl.postForm("/login", url.Values{
		"csrf_token": {token},
		"secret":     {secret},
	})
}

func TestLoginFlow(t *testing.T) {
	cl := newClient(t)

	// unauthenticated: protected page redirects to login
	if status, _ := cl.get("/"); status != http.StatusFound {
		t.Fatalf("GET / unauthenticated = %d, want 302", status)
	}

	status, body := cl.get("/login")
	if status != http.StatusOK || !strings.Contains(body, "pooml") {
		t.Fatalf("login page: status %d, pooml branding present: %v", status, strings.Contains(body, "pooml"))
	}

	// wrong secret: 401, friendly error, no session
	resp := cl.login("wrong-secret", cl.csrfToken())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong secret = %d, want 401", resp.StatusCode)
	}
	if status, _ := cl.get("/"); status != http.StatusFound {
		t.Fatal("wrong secret still created a session")
	}

	// correct secret: redirect home with session cookie
	resp = cl.login(testSecret, cl.csrfToken())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login = %d -> %q, want 303 -> /", resp.StatusCode, resp.Header.Get("Location"))
	}
	status, body = cl.get("/")
	if status != http.StatusOK || !strings.Contains(body, "Triage") {
		t.Fatalf("home after login: %d", status)
	}

	// authed visit to /login bounces home
	if status, _ := cl.get("/login"); status != http.StatusSeeOther {
		t.Errorf("GET /login while authed = %d, want 303", status)
	}

	// logout: session gone, cookie cleared
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no csrf token on home page (logout form)")
	}
	resp = cl.postForm("/logout", url.Values{"csrf_token": {m[1]}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", resp.StatusCode)
	}
	if status, _ := cl.get("/"); status != http.StatusFound {
		t.Error("session survived logout")
	}
}

func TestLoginThrottleLockout(t *testing.T) {
	cl := newClient(t)

	// 5 failures within the window trigger the lockout
	for range 5 {
		cl.login("wrong-secret", cl.csrfToken())
	}
	resp := cl.login(testSecret, cl.csrfToken())
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked-out login = %d, want 429 (even with the right secret)", resp.StatusCode)
	}
}

func TestCSRFFailureRedirectsToStaleLogin(t *testing.T) {
	cl := newClient(t)
	cl.get("/login") // set the csrf cookie

	resp := cl.postForm("/login", url.Values{
		"csrf_token": {"forged-token"},
		"secret":     {testSecret},
	})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "err=stale") {
		t.Fatalf("forged token = %d -> %q, want 303 -> /login?err=stale", resp.StatusCode, resp.Header.Get("Location"))
	}

	// and the stale page explains itself
	_, body := cl.get("/login?err=stale")
	if !strings.Contains(body, "stale") {
		t.Error("stale login page missing explanation")
	}
}

func TestSettingsApiKeysFlow(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	// settings page renders the shell with the api-keys section
	status, body := cl.get("/settings")
	if status != http.StatusOK || !strings.Contains(body, "API keys") {
		t.Fatalf("settings page: %d", status)
	}
	if !strings.Contains(body, "No keys yet") {
		t.Error("empty state missing")
	}

	// mint a key: plaintext shown exactly once
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no csrf token on settings page")
	}
	resp := cl.postForm("/settings/api-keys", url.Values{
		"csrf_token": {m[1]},
		"label":      {"fluentbit-test"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create key = %d", resp.StatusCode)
	}
	// re-fetch the body of the create response is gone (postForm closes it),
	// so mint again through a fresh page load to grab the key text
	_, body = cl.get("/settings")
	if strings.Contains(body, "pooml_") {
		t.Error("plaintext key visible on a later page load")
	}
	if !strings.Contains(body, "fluentbit-test") {
		t.Error("created key not listed")
	}

	// empty label is rejected with a friendly error
	m = csrfRe.FindStringSubmatch(body)
	resp = cl.postForm("/settings/api-keys", url.Values{
		"csrf_token": {m[1]},
		"label":      {"   "},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank label = %d, want 400", resp.StatusCode)
	}

	// revoke via the HTMX path: DELETE with the X-CSRF-Token header
	req, err := http.NewRequest(http.MethodDelete, cl.srv.URL+"/settings/api-keys/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", m[1])
	req.Header.Set("Origin", cl.srv.URL)
	delResp, err := cl.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fragment, _ := io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", delResp.StatusCode)
	}
	if !strings.Contains(string(fragment), "No keys yet") {
		t.Errorf("revoke fragment should show empty state, got: %.200s", fragment)
	}
}

func TestSecurityHeaders(t *testing.T) {
	cl := newClient(t)
	resp, err := cl.c.Get(cl.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "cdn.jsdelivr.net", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}
