package ui_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (cl *client) htmxForm(method, path, token string, form url.Values) (int, string) {
	cl.t.Helper()
	req, err := http.NewRequest(method, cl.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		cl.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", token)
	req.Header.Set("Origin", cl.srv.URL)
	req.Header.Set("HX-Request", "true")
	resp, err := cl.c.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestRetentionSettings(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	// defaults render before anything is saved
	status, page := cl.get("/settings")
	if status != http.StatusOK {
		t.Fatalf("settings page: %d", status)
	}
	token := csrfRe.FindStringSubmatch(page)[1]
	for _, want := range []string{`name="logs_days"`, `value="30"`, `value="90"`} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page missing %q", want)
		}
	}

	// valid save persists and confirms
	status, frag := cl.htmxForm(http.MethodPut, "/settings/retention", token, url.Values{
		"logs_days": {"7"}, "metrics_days": {"14"}, "alert_firings_days": {"21"},
	})
	if status != http.StatusOK || !strings.Contains(frag, "Saved") {
		t.Fatalf("valid save: %d, %.200s", status, frag)
	}
	if !strings.Contains(frag, `value="7"`) || !strings.Contains(frag, `value="14"`) {
		t.Errorf("saved fragment doesn't echo new values: %.300s", frag)
	}
	var stored string
	if err := cl.pools.Meta.QueryRow("SELECT value FROM settings WHERE key = 'retention.logs_days'").Scan(&stored); err != nil || stored != "7" {
		t.Errorf("stored logs_days = %q, err %v; want 7", stored, err)
	}

	// out-of-range value rejected, stored values untouched
	status, frag = cl.htmxForm(http.MethodPut, "/settings/retention", token, url.Values{
		"logs_days": {"0"}, "metrics_days": {"14"}, "alert_firings_days": {"21"},
	})
	if status != http.StatusOK || !strings.Contains(frag, "between 1 and 3650") {
		t.Fatalf("invalid save: %d, %.200s", status, frag)
	}
	if err := cl.pools.Meta.QueryRow("SELECT value FROM settings WHERE key = 'retention.logs_days'").Scan(&stored); err != nil || stored != "7" {
		t.Errorf("logs_days after rejected save = %q; want unchanged 7", stored)
	}

	// non-numeric rejected too
	if _, frag = cl.htmxForm(http.MethodPut, "/settings/retention", token, url.Values{
		"logs_days": {"forever"}, "metrics_days": {"14"}, "alert_firings_days": {"21"},
	}); !strings.Contains(frag, "between 1 and 3650") {
		t.Errorf("non-numeric save not rejected: %.200s", frag)
	}
}

func TestBackupSettings(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	status, page := cl.get("/settings")
	if status != http.StatusOK || !strings.Contains(page, "Backup") {
		t.Fatalf("settings page missing backup section: %d", status)
	}
	token := csrfRe.FindStringSubmatch(page)[1]

	// invalid cron rejected
	_, frag := cl.htmxForm(http.MethodPut, "/settings/backup", token, url.Values{
		"bucket": {"bkt"}, "schedule": {"whenever"},
	})
	if !strings.Contains(frag, "not a cron expression") {
		t.Fatalf("bad cron accepted: %.200s", frag)
	}

	// valid save persists; secrets stored encrypted
	_, frag = cl.htmxForm(http.MethodPut, "/settings/backup", token, url.Values{
		"bucket": {"bkt"}, "prefix": {"/pooml/"}, "schedule": {"0 4 * * *"},
		"access_key": {"AK"}, "secret_key": {"SK"}, "enabled": {"on"},
	})
	if !strings.Contains(frag, "Saved") || !strings.Contains(frag, `value="0 4 * * *"`) {
		t.Fatalf("valid save: %.300s", frag)
	}
	var stored, enc string
	if err := cl.pools.Meta.QueryRow("SELECT value FROM settings WHERE key = 'backup.s3_prefix'").Scan(&stored); err != nil || stored != "pooml" {
		t.Errorf("prefix = %q (err %v), want trimmed 'pooml'", stored, err)
	}
	row := cl.pools.Meta.QueryRow("SELECT value, is_encrypted FROM settings WHERE key = 'backup.s3_secret_key'")
	var isEnc bool
	if err := row.Scan(&enc, &isEnc); err != nil || !isEnc || enc == "SK" {
		t.Errorf("secret stored plaintext or missing: enc=%v val=%q err=%v", isEnc, enc, err)
	}

	// blank secrets on re-save keep the stored values
	_, _ = cl.htmxForm(http.MethodPut, "/settings/backup", token, url.Values{
		"bucket": {"bkt2"}, "schedule": {"0 4 * * *"},
	})
	var after string
	if err := cl.pools.Meta.QueryRow("SELECT value FROM settings WHERE key = 'backup.s3_secret_key'").Scan(&after); err != nil || after != enc {
		t.Errorf("blank secret save wiped the stored secret")
	}
	// unchecked checkbox turns the schedule off
	var enabled string
	if err := cl.pools.Meta.QueryRow("SELECT value FROM settings WHERE key = 'backup.enabled'").Scan(&enabled); err != nil || enabled != "false" {
		t.Errorf("enabled after unchecked save = %q, want false", enabled)
	}
}
