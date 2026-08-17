package ui_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func createAlertForm(name, query string) url.Values {
	return url.Values{
		"name":        {name},
		"query":       {query},
		"interval_s":  {"60"},
		"cooldown_m":  {"15"},
		"target_type": {"campfire"},
		"cf_room_id":  {"7"},
		"enabled":     {"on"},
	}
}

func TestAlertsCRUDFlow(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	status, body := cl.get("/alerts")
	if status != http.StatusOK || !strings.Contains(body, "No alerts yet") {
		t.Fatalf("alerts page: %d", status)
	}
	if !strings.Contains(body, "No notification channel configured") {
		t.Error("unconfigured state should point at Settings")
	}
	if strings.Contains(body, `<option value="pushover"`) || strings.Contains(body, `<option value="campfire"`) {
		t.Error("selector must not offer unconfigured channels")
	}

	// creating with an unconfigured channel is rejected
	m := csrfRe.FindStringSubmatch(body)
	rejected := createAlertForm("nope", "SELECT * FROM logs LIMIT 1")
	rejected.Set("csrf_token", m[1])
	if resp := cl.postForm("/alerts", rejected); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unconfigured channel create = %d, want 400", resp.StatusCode)
	}

	// configure campfire, then the selector offers it
	cl.postForm("/settings/campfire", url.Values{
		"csrf_token": {m[1]},
		"base_url":   {"https://chat.example.com"},
		"bot_key":    {"k"},
	})
	_, body = cl.get("/alerts")
	if !strings.Contains(body, `<option value="campfire"`) || strings.Contains(body, `<option value="pushover"`) {
		t.Fatalf("selector should offer exactly the configured channel")
	}
	if !strings.Contains(body, "sql-editor-host") {
		t.Error("query editor host missing")
	}

	// create
	m = csrfRe.FindStringSubmatch(body)
	form := createAlertForm("errors present", "SELECT * FROM logs WHERE level >= 4")
	form.Set("csrf_token", m[1])
	resp := cl.postForm("/alerts", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	_, body = cl.get("/alerts")
	if !strings.Contains(body, "errors present") || !strings.Contains(body, "campfire · room 7") {
		t.Fatalf("created alert not listed: %.300s", body)
	}
	if !strings.Contains(body, "quiet") {
		t.Error("fresh alert should show quiet status")
	}

	// invalid create: bad query is rejected with the error banner
	m = csrfRe.FindStringSubmatch(body)
	badForm := createAlertForm("bad", "DROP TABLE logs")
	badForm.Set("csrf_token", m[1])
	resp = cl.postForm("/alerts", badForm)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad query create = %d, want 400", resp.StatusCode)
	}

	// dry run: seeded error row means it would fire
	seedLogs(cl, 2) // one error row (auth-svc level 4)
	req, _ := http.NewRequest(http.MethodPost, cl.srv.URL+"/alerts/1/test", nil)
	req.Header.Set("X-CSRF-Token", m[1])
	req.Header.Set("Origin", cl.srv.URL)
	dresp, err := cl.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dbody := readBody(t, dresp)
	if !strings.Contains(dbody, "WOULD fire") || !strings.Contains(dbody, "no notification sent") {
		t.Errorf("dry run: %.300s", dbody)
	}

	// edit fragment prefills the form
	eresp := cl.getWith("/alerts/1/edit", map[string]string{"HX-Request": "true"})
	ebody := readBody(t, eresp)
	if !strings.Contains(ebody, `value="errors present"`) || !strings.Contains(ebody, "SELECT * FROM logs WHERE level &gt;= 4") {
		t.Errorf("edit form: %.400s", ebody)
	}

	// delete via HTMX
	dreq, _ := http.NewRequest(http.MethodDelete, cl.srv.URL+"/alerts/1", nil)
	dreq.Header.Set("X-CSRF-Token", m[1])
	dreq.Header.Set("Origin", cl.srv.URL)
	delResp, err := cl.c.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	delBody := readBody(t, delResp)
	if delResp.StatusCode != http.StatusOK || !strings.Contains(delBody, "No alerts yet") {
		t.Errorf("delete: %d %.200s", delResp.StatusCode, delBody)
	}
}

func TestSettingsChannels(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	_, body := cl.get("/settings")
	if !strings.Contains(body, "Pushover") || !strings.Contains(body, "Campfire") {
		t.Fatal("channel sections missing")
	}
	if strings.Count(body, "not configured") < 2 {
		t.Error("both channels should start unconfigured")
	}

	// save campfire config
	m := csrfRe.FindStringSubmatch(body)
	resp := cl.postForm("/settings/campfire", url.Values{
		"csrf_token": {m[1]},
		"base_url":   {"https://chat.example.com"},
		"bot_key":    {"secret-bot-key"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save campfire = %d", resp.StatusCode)
	}
	_, body = cl.get("/settings")
	if !strings.Contains(body, `value="https://chat.example.com"`) {
		t.Error("base url not shown after save")
	}
	if strings.Contains(body, "secret-bot-key") {
		t.Error("bot key echoed back to the page")
	}
	if !strings.Contains(body, "configured - enter to replace") {
		t.Error("secret placeholder missing after configuration")
	}

	// unconfigured pushover test returns the friendly error fragment
	treq, _ := http.NewRequest(http.MethodPost, cl.srv.URL+"/settings/pushover/test", nil)
	treq.Header.Set("X-CSRF-Token", m[1])
	treq.Header.Set("Origin", cl.srv.URL)
	tresp, err := cl.c.Do(treq)
	if err != nil {
		t.Fatal(err)
	}
	tbody := readBody(t, tresp)
	if !strings.Contains(tbody, "not configured") {
		t.Errorf("pushover test fragment: %.200s", tbody)
	}
}

func TestAlertWarRoomToggleRoundTrip(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	_, body := cl.get("/alerts")
	m := csrfRe.FindStringSubmatch(body)
	cl.postForm("/settings/campfire", url.Values{
		"csrf_token": {m[1]},
		"base_url":   {"https://chat.example.com"},
		"bot_key":    {"k"},
	})
	_, body = cl.get("/alerts")
	if !strings.Contains(body, "Include a War Room link") {
		t.Fatal("war room toggle missing from the alert form")
	}
	// PublicURL unset in tests: the form must say what's missing
	if !strings.Contains(body, "POOML_PUBLIC_URL") {
		t.Error("unavailable state should name the env var")
	}

	m = csrfRe.FindStringSubmatch(body)
	form := createAlertForm("with link", "SELECT * FROM logs WHERE level >= 4")
	form.Set("csrf_token", m[1])
	form.Set("war_room", "on")
	if resp := cl.postForm("/alerts", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	var target string
	if err := cl.pools.Meta.QueryRow("SELECT target FROM alerts WHERE name = 'with link'").Scan(&target); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target, `"war_room":true`) {
		t.Errorf("stored target = %s", target)
	}

	// the edit form restores the checked state
	eresp := cl.getWith("/alerts/1/edit", map[string]string{"HX-Request": "true"})
	ebody := readBody(t, eresp)
	if !strings.Contains(ebody, `name="war_room" checked`) {
		t.Errorf("edit form loses the toggle: %.300s", ebody)
	}
}
