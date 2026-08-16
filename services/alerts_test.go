package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/query"
)

func newTestPools(t *testing.T) *db.Pools {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	return pools
}

func TestEncryptionRoundTrip(t *testing.T) {
	pools := newTestPools(t)
	enc, err := NewEncryptionService("test-encryption-key-0123456789012345", pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := enc.Encrypt("s3cret value")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, "s3cret") {
		t.Error("ciphertext leaks plaintext")
	}
	got, err := enc.Decrypt(ct)
	if err != nil || got != "s3cret value" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}

	// wrong key must fail, not return garbage
	enc2, _ := NewEncryptionService("another-key-entirely-9876543210987654", pools.Meta)
	if _, err := enc2.Decrypt(ct); err == nil {
		t.Error("decrypt with wrong key succeeded")
	}
	// tampering must fail
	tampered := ct[:len(ct)-4] + "AAA="
	if _, err := enc.Decrypt(tampered); err == nil {
		t.Error("tampered ciphertext decrypted")
	}
}

func TestSettingsEncryptedStorage(t *testing.T) {
	pools := newTestPools(t)
	enc, _ := NewEncryptionService("test-encryption-key-0123456789012345", pools.Meta)
	s := NewSettingsService(pools.Meta, enc)
	ctx := context.Background()

	if v, err := s.Get(ctx, SettingPushoverAppToken); err != nil || v != "" {
		t.Fatalf("unset key = %q, %v", v, err)
	}
	if s.IsSet(ctx, SettingPushoverAppToken) {
		t.Error("IsSet true for unset key")
	}

	if err := s.Set(ctx, SettingPushoverAppToken, "app-token-123", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, SettingCampfireBaseURL, "https://chat.example.com", false); err != nil {
		t.Fatal(err)
	}

	// encrypted value never stored in the clear
	var stored string
	if err := pools.Meta.QueryRow("SELECT value FROM settings WHERE key = ?", SettingPushoverAppToken).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "app-token-123") {
		t.Error("secret stored in plaintext")
	}

	if v, _ := s.Get(ctx, SettingPushoverAppToken); v != "app-token-123" {
		t.Errorf("decrypted = %q", v)
	}
	if v, _ := s.Get(ctx, SettingCampfireBaseURL); v != "https://chat.example.com" {
		t.Errorf("plain = %q", v)
	}

	// upsert replaces
	_ = s.Set(ctx, SettingPushoverAppToken, "replaced", true)
	if v, _ := s.Get(ctx, SettingPushoverAppToken); v != "replaced" {
		t.Errorf("after upsert = %q", v)
	}
}

func newNotifyFixture(t *testing.T) (*NotificationService, *SettingsService) {
	pools := newTestPools(t)
	enc, _ := NewEncryptionService("test-encryption-key-0123456789012345", pools.Meta)
	settings := NewSettingsService(pools.Meta, enc)
	return NewNotificationService(settings), settings
}

func TestPushoverSend(t *testing.T) {
	n, settings := newNotifyFixture(t)
	ctx := context.Background()

	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()
	n.PushoverBaseURL = srv.URL

	// unconfigured: clear error
	if err := n.TestPushover(ctx); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured err = %v", err)
	}

	_ = settings.Set(ctx, SettingPushoverAppToken, "tok", true)
	_ = settings.Set(ctx, SettingPushoverUserKey, "usr", true)

	res := &query.Result{Columns: []string{"service", "message"}, Rows: [][]any{{"payment-svc", "boom"}}}
	if err := n.SendAlert(ctx, "High errors", `{"type":"pushover","priority":1,"device":"phone"}`, res); err != nil {
		t.Fatal(err)
	}
	if gotForm["token"][0] != "tok" || gotForm["user"][0] != "usr" {
		t.Errorf("credentials not sent: %v", gotForm)
	}
	if gotForm["priority"][0] != "1" || gotForm["device"][0] != "phone" {
		t.Errorf("target options not sent: %v", gotForm)
	}
	if !strings.Contains(gotForm["message"][0], "payment-svc") {
		t.Errorf("message missing rows: %q", gotForm["message"][0])
	}

	// pushover-level rejection (status 0) is an error even on HTTP 200
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":0,"errors":["user identifier is invalid"]}`))
	}))
	defer srv2.Close()
	n.PushoverBaseURL = srv2.URL
	if err := n.TestPushover(ctx); err == nil || !strings.Contains(err.Error(), "user identifier") {
		t.Errorf("status-0 err = %v", err)
	}
}

func TestCampfireSend(t *testing.T) {
	n, settings := newNotifyFixture(t)
	ctx := context.Background()

	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 4096)
		nn, _ := r.Body.Read(b)
		gotBody = string(b[:nn])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	_ = settings.Set(ctx, SettingCampfireBaseURL, srv.URL, false)
	_ = settings.Set(ctx, SettingCampfireBotKey, "bot-key-secret", true)

	res := &query.Result{Columns: []string{"message"}, Rows: [][]any{{`<script>alert(1)</script>`}}}
	if err := n.SendAlert(ctx, "XSS <alert>", `{"type":"campfire","room_id":42}`, res); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/rooms/42/bot-key-secret/messages" {
		t.Errorf("path = %q", gotPath)
	}
	// log content and alert name must arrive escaped
	if strings.Contains(gotBody, "<script>") || !strings.Contains(gotBody, "&lt;script&gt;") {
		t.Errorf("unescaped content in body: %q", gotBody)
	}
	if !strings.Contains(gotBody, "XSS &lt;alert&gt;") {
		t.Errorf("alert name not escaped: %q", gotBody)
	}

	// the 3xx trap: a redirect is NOT success, and the error must not leak the key
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv3.Close()
	// no CheckRedirect override here: the PRODUCTION client must refuse to
	// follow the redirect, or this test masks a silently-lost notification
	_ = settings.Set(ctx, SettingCampfireBaseURL, srv3.URL, false)
	err := n.TestCampfire(ctx, 42)
	if err == nil {
		t.Fatal("3xx treated as success - lost notification counted as delivered")
	}
	if strings.Contains(err.Error(), "bot-key-secret") {
		t.Error("error message leaks the bot key")
	}
}

// recorder implements AlertNotifier for evaluator tests.
type recorder struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (r *recorder) SendAlert(ctx context.Context, name, target string, res *query.Result) error {
	r.calls.Add(1)
	if r.fail.Load() {
		return context.DeadlineExceeded
	}
	return nil
}

func TestEvaluatorLifecycle(t *testing.T) {
	pools := newTestPools(t)
	store := NewAlertsService(pools.Meta)
	rec := &recorder{}
	ev := NewEvaluator(store, pools.LogsRead, rec)
	ctx := context.Background()

	// alert with a large cooldown; matching row exists
	if _, err := pools.LogsWrite.Exec(
		`INSERT INTO logs (timestamp, ingested_at, level, service, host, raw) VALUES (?, ?, 4, 'svc', 'h', 'err row')`,
		time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	err := store.Create(ctx, Alert{
		Name: "errors present", Query: "SELECT * FROM logs WHERE level >= 4",
		CheckIntervalMs: MinCheckIntervalMs, CooldownMs: 60 * 60 * 1000,
		Target: `{"type":"campfire","room_id":1}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	tickAndWait := func() {
		ev.tick(ctx)
		ev.wg.Wait()
	}

	// first tick: due (never checked), fires, notifies
	tickAndWait()
	if got := rec.calls.Load(); got != 1 {
		t.Fatalf("notifications after first tick = %d, want 1", got)
	}
	alerts, _ := store.List(ctx)
	a := alerts[0]
	if !a.CurrentlyFiring || !a.LastFiredAt.Valid || a.LastError.Valid {
		t.Fatalf("state after fire: firing=%v fired=%v err=%v", a.CurrentlyFiring, a.LastFiredAt.Valid, a.LastError)
	}

	// force due again: cooldown suppresses the second notification
	if _, err := pools.Meta.Exec("UPDATE alerts SET last_checked_at = last_checked_at - ?", MinCheckIntervalMs+1000); err != nil {
		t.Fatal(err)
	}
	tickAndWait()
	if got := rec.calls.Load(); got != 1 {
		t.Errorf("cooldown violated: notifications = %d", got)
	}
	alerts, _ = store.List(ctx)
	if !alerts[0].CurrentlyFiring {
		t.Error("currently_firing must stay true during cooldown suppression")
	}

	// condition clears -> firing clears
	if _, err := pools.LogsWrite.Exec("DELETE FROM logs"); err != nil {
		t.Fatal(err)
	}
	_, _ = pools.Meta.Exec("UPDATE alerts SET last_checked_at = last_checked_at - ?", MinCheckIntervalMs+1000)
	tickAndWait()
	alerts, _ = store.List(ctx)
	if alerts[0].CurrentlyFiring {
		t.Error("firing should clear when the query returns no rows")
	}

	// audit row was written exactly once
	var firings int
	_ = pools.Meta.QueryRow("SELECT COUNT(*) FROM alert_firings").Scan(&firings)
	if firings != 1 {
		t.Errorf("alert_firings rows = %d, want 1", firings)
	}
}

func TestEvaluatorNotifyFailureRetries(t *testing.T) {
	pools := newTestPools(t)
	store := NewAlertsService(pools.Meta)
	rec := &recorder{}
	rec.fail.Store(true)
	ev := NewEvaluator(store, pools.LogsRead, rec)
	ctx := context.Background()

	_, _ = pools.LogsWrite.Exec(
		`INSERT INTO logs (timestamp, ingested_at, level, service, host, raw) VALUES (1, 1, 4, 'svc', 'h', 'x')`)
	_ = store.Create(ctx, Alert{
		Name: "flaky notify", Query: "SELECT * FROM logs WHERE level >= 4",
		CheckIntervalMs: MinCheckIntervalMs, CooldownMs: 60 * 60 * 1000,
		Target: `{"type":"pushover"}`, Enabled: true,
	})

	ev.tick(ctx)
	ev.wg.Wait()
	alerts, _ := store.List(ctx)
	a := alerts[0]
	if a.LastFiredAt.Valid {
		t.Error("last_fired_at advanced despite delivery failure")
	}
	if !a.LastError.Valid || !strings.Contains(a.LastError.String, "notification failed") {
		t.Errorf("last_error = %v", a.LastError)
	}

	// delivery recovers: next due cycle retries (cooldown not started)
	rec.fail.Store(false)
	_, _ = pools.Meta.Exec("UPDATE alerts SET last_checked_at = last_checked_at - ?", MinCheckIntervalMs+1000)
	ev.tick(ctx)
	ev.wg.Wait()
	if got := rec.calls.Load(); got != 2 {
		t.Errorf("retry after failure: calls = %d, want 2", got)
	}
	alerts, _ = store.List(ctx)
	if !alerts[0].LastFiredAt.Valid || alerts[0].LastError.Valid {
		t.Errorf("recovery state: fired=%v err=%v", alerts[0].LastFiredAt.Valid, alerts[0].LastError)
	}
}

func TestAlertValidation(t *testing.T) {
	pools := newTestPools(t)
	store := NewAlertsService(pools.Meta)
	ctx := context.Background()

	base := Alert{
		Name: "ok", Query: "SELECT * FROM logs LIMIT 1",
		CheckIntervalMs: MinCheckIntervalMs, CooldownMs: 0,
		Target: `{"type":"campfire","room_id":1}`,
	}
	bad := []func(a Alert) Alert{
		func(a Alert) Alert { a.Name = ""; return a },
		func(a Alert) Alert { a.Query = "DROP TABLE logs"; return a },
		func(a Alert) Alert { a.Query = "SELECT * FROM sqlite_master"; return a },
		func(a Alert) Alert { a.CheckIntervalMs = 5000; return a },
		func(a Alert) Alert { a.CooldownMs = -1; return a },
		func(a Alert) Alert { a.Target = `{"type":"slack"}`; return a },
		func(a Alert) Alert { a.Target = `{"type":"campfire"}`; return a },
		func(a Alert) Alert { a.Target = `{"type":"pushover","priority":9}`; return a },
	}
	for i, mut := range bad {
		if err := store.Create(ctx, mut(base)); err == nil {
			t.Errorf("bad alert %d accepted", i)
		}
	}
	if err := store.Create(ctx, base); err != nil {
		t.Errorf("valid alert rejected: %v", err)
	}
}
