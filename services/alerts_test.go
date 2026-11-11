package services

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	if err := n.SendAlert(ctx, Alert{Name: "High errors", Target: `{"type":"pushover","priority":1,"device":"phone"}`}, res); err != nil {
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
	if err := n.SendAlert(ctx, Alert{Name: "XSS <alert>", Target: `{"type":"campfire","room_id":42}`}, res); err != nil {
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

// The "flying blind" message goes to the alert's own target, escaped for
// Campfire like any other content: the reason can quote query text.
func TestSendAlertFailure(t *testing.T) {
	n, settings := newNotifyFixture(t)
	n.PublicURL = "https://logs.example.com/"
	ctx := context.Background()
	since := time.Date(2026, 11, 10, 22, 15, 0, 0, time.UTC).UnixMilli()
	reason := `table "<b>x</b>" is not allowed ` + strings.Repeat("y", 1000)

	var campfireBody string
	campfire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		campfireBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer campfire.Close()
	_ = settings.Set(ctx, SettingCampfireBaseURL, campfire.URL, false)
	_ = settings.Set(ctx, SettingCampfireBotKey, "bot-key-secret", true)

	a := Alert{Name: "5xx <spike>", Target: `{"type":"campfire","room_id":7}`}
	if err := n.SendAlertFailure(ctx, a, reason, since); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"5xx &lt;spike&gt; is flying blind", "2026-11-10 22:15 UTC", "&lt;b&gt;x&lt;/b&gt;", `href="https://logs.example.com/alerts"`} {
		if !strings.Contains(campfireBody, want) {
			t.Errorf("campfire body missing %q: %.300q", want, campfireBody)
		}
	}
	if strings.Contains(campfireBody, "<b>x</b>") {
		t.Errorf("reason not escaped: %.300q", campfireBody)
	}
	if len(campfireBody) > 1000 {
		t.Errorf("reason not truncated: body is %d bytes", len(campfireBody))
	}

	var form map[string][]string
	pushover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Write([]byte(`{"status":1}`))
	}))
	defer pushover.Close()
	n.PushoverBaseURL = pushover.URL
	_ = settings.Set(ctx, SettingPushoverAppToken, "tok", true)
	_ = settings.Set(ctx, SettingPushoverUserKey, "usr", true)

	a.Target = `{"type":"pushover","priority":1}`
	if err := n.SendAlertFailure(ctx, a, "query failed: interrupted", since); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(form["title"][0], "5xx <spike> is flying blind") || !strings.Contains(form["message"][0], "query failed: interrupted") {
		t.Errorf("pushover content: title=%q message=%q", form["title"], form["message"])
	}
	if form["priority"][0] != "1" || form["url"][0] != "https://logs.example.com/alerts" || form["url_title"][0] != "Open alerts" {
		t.Errorf("pushover routing/link: %v", form)
	}
}

// Pushover documents hard limits but not what it does past them, so pooml
// must never send more: a rejected notification is a lost alert.
func TestPushoverStaysWithinLimits(t *testing.T) {
	n, settings := newNotifyFixture(t)
	n.PublicURL = "https://logs.example.com"
	ctx := context.Background()

	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()
	n.PushoverBaseURL = srv.URL
	_ = settings.Set(ctx, SettingPushoverAppToken, "tok", true)
	_ = settings.Set(ctx, SettingPushoverUserKey, "usr", true)

	// six wide rows of multi-byte text, and a query long enough to overflow the URL
	wide := strings.Repeat("é", 400)
	res := &query.Result{Columns: []string{"message"}}
	for range 6 {
		res.Rows = append(res.Rows, []any{wide})
	}
	longQuery := "SELECT * FROM logs WHERE level >= 4 AND (" + strings.Repeat("message LIKE '%timeout%' OR ", 30) + "1 = 0)"
	a := Alert{Name: strings.Repeat("n", 100), Query: longQuery, Target: `{"type":"pushover","war_room":true}`}
	if err := n.SendAlert(ctx, a, res); err != nil {
		t.Fatal(err)
	}
	if got := utf8.RuneCountInString(form["message"][0]); got > 1024 {
		t.Errorf("message is %d characters, limit 1024", got)
	}
	if got := utf8.RuneCountInString(form["title"][0]); got > 250 {
		t.Errorf("title is %d characters, limit 250", got)
	}
	link := form["url"][0]
	if len(link) > 512 {
		t.Errorf("url is %d characters, limit 512", len(link))
	}
	if !strings.Contains(link, "/war-room?from=") || strings.Contains(link, "q=") {
		t.Errorf("an over-long link must keep the time window and drop the query: %q", link)
	}

	// a short query still travels in the link
	a.Query = "SELECT * FROM logs WHERE level >= 4"
	if err := n.SendAlert(ctx, a, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(form["url"][0], "q=SELECT") {
		t.Errorf("short query missing from the link: %q", form["url"][0])
	}
}

// recorder implements AlertNotifier for evaluator tests.
type recorder struct {
	calls        atomic.Int64
	fail         atomic.Bool
	failureCalls atomic.Int64
	failureFail  atomic.Bool
	lastReason   atomic.Value
}

func (r *recorder) SendAlertFailure(ctx context.Context, a Alert, reason string, failingSinceMs int64) error {
	if r.failureFail.Load() {
		return context.DeadlineExceeded
	}
	r.failureCalls.Add(1)
	r.lastReason.Store(reason)
	return nil
}

func (r *recorder) SendAlert(ctx context.Context, a Alert, res *query.Result) error {
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

// Two ways a stored query stops working. One no longer validates (what an
// upgrade that tightens the validator does): deterministic. The other validates
// but errors when run (a timeout, in real life): possibly a blip.
const (
	goodQuery      = "SELECT * FROM logs WHERE level >= 4"
	invalidQuery   = "SELECT * FROM sqlite_master"
	execFailsQuery = "SELECT abs(-9223372036854775807 - 1)"
)

type blindFixture struct {
	t     *testing.T
	pools *db.Pools
	store *AlertsService
	rec   *recorder
	ev    *Evaluator
}

func newBlindFixture(t *testing.T, cooldownMs int64) *blindFixture {
	t.Helper()
	pools := newTestPools(t)
	f := &blindFixture{t: t, pools: pools, store: NewAlertsService(pools.Meta), rec: &recorder{}}
	f.ev = NewEvaluator(f.store, pools.LogsRead, f.rec)
	if err := f.store.Create(context.Background(), Alert{
		Name: "watcher", Query: goodQuery, CheckIntervalMs: MinCheckIntervalMs, CooldownMs: cooldownMs,
		Target: `{"type":"pushover"}`, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// check makes the alert due, swaps in q, runs one evaluation and returns the row.
func (f *blindFixture) check(q string) Alert {
	f.t.Helper()
	if _, err := f.pools.Meta.Exec("UPDATE alerts SET query = ?, last_checked_at = last_checked_at - ?", q, MinCheckIntervalMs+1000); err != nil {
		f.t.Fatal(err)
	}
	f.ev.tick(context.Background())
	f.ev.wg.Wait()
	return f.alert()
}

func (f *blindFixture) alert() Alert {
	f.t.Helper()
	alerts, err := f.store.List(context.Background())
	if err != nil || len(alerts) != 1 {
		f.t.Fatalf("list: %v (%d alerts)", err, len(alerts))
	}
	return alerts[0]
}

// age moves the streak's clock back, as if ms had passed.
func (f *blindFixture) age(ms int64) {
	f.t.Helper()
	if _, err := f.pools.Meta.Exec(
		"UPDATE alerts SET failing_since = failing_since - ?, failure_notified_at = failure_notified_at - ?", ms, ms); err != nil {
		f.t.Fatal(err)
	}
}

func (f *blindFixture) announced() int64 { return f.rec.failureCalls.Load() }

// A query that no longer validates fails the same way on every check, so the
// first failure is announced: waiting for more would keep an hourly alert
// silent for hours over something that cannot fix itself.
func TestBlindAlertInvalidQueryAnnouncedAtOnce(t *testing.T) {
	f := newBlindFixture(t, 60*60*1000)

	a := f.check(invalidQuery)
	if f.announced() != 1 {
		t.Fatalf("announcements after the first failed check = %d, want 1", f.announced())
	}
	if reason, _ := f.rec.lastReason.Load().(string); !strings.Contains(reason, "sqlite_master") {
		t.Errorf("reason = %q, want the validation error", reason)
	}
	if a.FailureCount != 1 || !a.FailingSince.Valid || !a.FailureNotifiedAt.Valid || !a.QueryBroken() {
		t.Fatalf("state: count=%d since=%v notified=%v broken=%v", a.FailureCount, a.FailingSince, a.FailureNotifiedAt, a.QueryBroken())
	}

	// still broken: quiet inside the cooldown, repeated after it
	f.check(invalidQuery)
	if f.announced() != 1 {
		t.Fatalf("repeated inside the cooldown: %d", f.announced())
	}
	f.age(60*60*1000 + 1000)
	f.check(invalidQuery)
	if f.announced() != 2 {
		t.Fatalf("not repeated after the cooldown: %d", f.announced())
	}

	// fixed: the streak ends without ceremony
	a = f.check(goodQuery)
	if a.FailureCount != 0 || a.FailingSince.Valid || a.LastError.Valid || a.QueryBroken() {
		t.Fatalf("streak not cleared: count=%d since=%v err=%v", a.FailureCount, a.FailingSince, a.LastError)
	}
}

// A query that errors when run may just have hit a slow moment: it takes a
// second failed check AND five minutes before anyone is told.
func TestBlindAlertExecutionErrorIsDebounced(t *testing.T) {
	f := newBlindFixture(t, 0)

	a := f.check(execFailsQuery)
	if !a.LastError.Valid || !strings.Contains(a.LastError.String, "query failed") {
		t.Fatalf("the test query must fail at execution, not validation: %v", a.LastError)
	}
	if f.announced() != 0 || a.QueryBroken() {
		t.Fatalf("one failed check: announced=%d broken=%v, want neither", f.announced(), a.QueryBroken())
	}

	// a single failure long ago is still a single failure
	f.age(failureNotifyAfterMs * 10)
	f.check(goodQuery)
	f.check(execFailsQuery)
	if f.announced() != 0 {
		t.Fatalf("a recovered streak counted towards the next one: %d", f.announced())
	}

	// second check in a row, but minutes have not passed
	a = f.check(execFailsQuery)
	if f.announced() != 0 || !a.QueryBroken() {
		t.Fatalf("two quick failures: announced=%d broken=%v, want 0 and true", f.announced(), a.QueryBroken())
	}

	// the channel is down when the threshold is reached: not marked, retried
	f.rec.failureFail.Store(true)
	f.age(failureNotifyAfterMs + 1000)
	if a = f.check(execFailsQuery); a.FailureNotifiedAt.Valid {
		t.Fatal("marked as announced although the send failed")
	}
	f.rec.failureFail.Store(false)
	f.check(execFailsQuery)
	if f.announced() != 1 {
		t.Fatalf("announcement not retried after a failed send: %d", f.announced())
	}

	// zero cooldown must not mean "every check": five minutes is the floor
	f.check(execFailsQuery)
	if f.announced() != 1 {
		t.Fatalf("zero cooldown repeated on the very next check: %d", f.announced())
	}
}

// The cooldown has to span streaks: an alert that flaps (fails for a while,
// one good check, fails again) must not announce every time it re-breaks.
func TestBlindAlertFlappingRespectsCooldown(t *testing.T) {
	f := newBlindFixture(t, 24*60*60*1000)
	for cycle := range 3 {
		f.check(execFailsQuery)
		f.age(failureNotifyAfterMs + 1000)
		f.check(execFailsQuery)
		if f.announced() != 1 {
			t.Fatalf("cycle %d: %d announcements inside a 24h cooldown, want 1", cycle, f.announced())
		}
		f.check(goodQuery)
	}
}

// Editing an alert (which is also how it is disabled and re-enabled) starts it
// over: a streak from before must not page on the first check afterwards.
func TestAlertUpdateResetsFailureStreak(t *testing.T) {
	f := newBlindFixture(t, 0)
	ctx := context.Background()
	weekAgo := time.Now().UnixMilli() - 7*24*60*60*1000
	if _, err := f.pools.Meta.Exec(
		`UPDATE alerts SET failure_count = 9, failing_since = ?, failure_notified_at = ?, last_error = 'old', last_error_at = 1`,
		weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	a := f.alert()
	a.Enabled = false
	if err := f.store.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Enabled = true
	if err := f.store.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := f.alert(); got.FailureCount != 0 || got.FailingSince.Valid || got.FailureNotifiedAt.Valid || got.LastError.Valid {
		t.Fatalf("Update kept stale state: %+v", got)
	}
	f.check(execFailsQuery)
	if f.announced() != 0 {
		t.Fatalf("announced on the first failed check after re-enabling: %d", f.announced())
	}
}

// The evaluator holds a copy of the alert from before the check ran. If the
// alert is edited while that check is in flight, the verdict must not be
// decided from the copy: it used to announce seconds after the admin's fix.
func TestBlindAlertIgnoresStaleCopy(t *testing.T) {
	f := newBlindFixture(t, 0)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// what the evaluator loaded: a long streak, due for an announcement
	stale := f.alert()
	stale.Query = execFailsQuery
	stale.FailureCount = 9
	stale.FailingSince = sql.NullInt64{Int64: now - failureNotifyAfterMs*10, Valid: true}

	// meanwhile the admin replaced the query: the late verdict is about a
	// query that no longer exists and must change nothing
	f.ev.recordError(ctx, stale, "query failed: late", now, false)
	if a := f.alert(); f.announced() != 0 || a.FailureCount != 0 || a.LastError.Valid {
		t.Fatalf("a check of the old query touched the edited alert: announced=%d count=%d err=%v", f.announced(), a.FailureCount, a.LastError)
	}

	// same query, but the streak was reset under it (any other field edited):
	// the database says this is failure one, and that decides
	if _, err := f.pools.Meta.Exec("UPDATE alerts SET query = ?", execFailsQuery); err != nil {
		t.Fatal(err)
	}
	f.ev.recordError(ctx, stale, "query failed: late", now, false)
	if a := f.alert(); f.announced() != 0 || a.FailureCount != 1 {
		t.Fatalf("decided from the stale copy: announced=%d count=%d, want 0 and 1", f.announced(), a.FailureCount)
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

func TestWarRoomLinkInNotifications(t *testing.T) {
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
	_ = settings.Set(ctx, SettingPushoverAppToken, "tok", true)
	_ = settings.Set(ctx, SettingPushoverUserKey, "usr", true)

	a := Alert{
		Name:   "High errors",
		Query:  "SELECT * FROM logs WHERE level >= 4",
		Target: `{"type":"pushover","war_room":true}`,
	}
	res := &query.Result{Columns: []string{"raw"}, Rows: [][]any{{"boom"}}}

	// toggle on but no public URL: no link (pooml can't guess its address)
	if err := n.SendAlert(ctx, a, res); err != nil {
		t.Fatal(err)
	}
	if len(gotForm["url"]) != 0 {
		t.Errorf("link sent without PublicURL: %v", gotForm["url"])
	}

	n.PublicURL = "https://logs.example.com"
	if err := n.SendAlert(ctx, a, res); err != nil {
		t.Fatal(err)
	}
	link := gotForm["url"][0]
	if !strings.HasPrefix(link, "https://logs.example.com/war-room?") {
		t.Errorf("link = %q", link)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("q") != a.Query {
		t.Errorf("link q = %q", u.Query().Get("q"))
	}
	from, _ := strconv.ParseInt(u.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(u.Query().Get("to"), 10, 64)
	if to-from != 30*60*1000 {
		t.Errorf("window span = %dms, want 30m centered on firing", to-from)
	}
	if gotForm["url_title"][0] == "" {
		t.Error("url_title missing")
	}

	// toggle off: no link even with PublicURL set
	a.Target = `{"type":"pushover"}`
	if err := n.SendAlert(ctx, a, res); err != nil {
		t.Fatal(err)
	}
	if len(gotForm["url"]) != 0 {
		t.Errorf("link sent with the toggle off: %v", gotForm["url"])
	}
}
