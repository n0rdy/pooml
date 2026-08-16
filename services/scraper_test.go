package services_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/metrics"
	"github.com/n0rdy/pooml/services"
)

type rowRecorder struct {
	mu     sync.Mutex
	rows   []metrics.Row
	reject bool
}

func (r *rowRecorder) TryPush(rows []metrics.Row) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reject {
		return false
	}
	r.rows = append(r.rows, rows...)
	return true
}

func (r *rowRecorder) snapshot() []metrics.Row {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]metrics.Row(nil), r.rows...)
}

func scrapeSetup(t *testing.T) (*services.ScrapeTargetsService, *rowRecorder, *services.Scraper) {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	enc, err := services.NewEncryptionService(strings.Repeat("k", 32), pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	store := services.NewScrapeTargetsService(pools.Meta, enc)
	rec := &rowRecorder{}
	return store, rec, services.NewScraper(store, rec)
}

const promPayload = `# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{code="200"} 1027
http_requests_total{code="500"} 3
# TYPE queue_depth gauge
queue_depth 7
# TYPE req_duration_seconds histogram
req_duration_seconds_bucket{le="0.1"} 100
req_duration_seconds_bucket{le="+Inf"} 120
req_duration_seconds_sum 9.5
req_duration_seconds_count 120
some_untyped_metric 42
`

func target(url string) services.ScrapeTarget {
	return services.ScrapeTarget{
		Service:          "shop",
		Host:             "h1",
		URL:              url,
		ScrapeIntervalMs: services.MinScrapeIntervalMs,
		Enabled:          true,
	}
}

func createAndGet(t *testing.T, store *services.ScrapeTargetsService, st services.ScrapeTarget) services.ScrapeTarget {
	t.Helper()
	if err := store.Create(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	targets, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range targets {
		if got.URL == st.URL {
			return got
		}
	}
	t.Fatalf("created target %q not found", st.URL)
	return services.ScrapeTarget{}
}

func waitScraped(t *testing.T, store *services.ScrapeTargetsService, id int64) services.ScrapeTarget {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		targets, err := store.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range targets {
			if got.ID == id && got.LastScrapedAt.Valid {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("target never scraped")
	return services.ScrapeTarget{}
}

func runScraper(t *testing.T, s *services.Scraper) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("scraper did not stop")
		}
	})
}

func TestScraperEndToEnd(t *testing.T) {
	store, rec, scraper := scrapeSetup(t)

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("X-API-Key")
		w.Write([]byte(promPayload))
	}))
	t.Cleanup(srv.Close)

	st := target(srv.URL)
	st.AuthHeader = "X-API-Key: sekret"
	created := createAndGet(t, store, st)
	if created.AuthHeader != "X-API-Key: sekret" {
		t.Fatalf("auth header round trip: %q", created.AuthHeader)
	}

	runScraper(t, scraper)
	after := waitScraped(t, store, created.ID)
	if after.LastError.Valid {
		t.Fatalf("unexpected scrape error: %s", after.LastError.String)
	}
	if gotAuth != "sekret" {
		t.Fatalf("auth header not sent: %q", gotAuth)
	}

	byName := map[string][]metrics.Row{}
	for _, r := range rec.snapshot() {
		byName[r.Name] = append(byName[r.Name], r)
	}
	if len(byName["http_requests_total"]) != 2 || byName["http_requests_total"][0].Type != metrics.TypeCounter {
		t.Fatalf("counter rows wrong: %+v", byName["http_requests_total"])
	}
	if len(byName["queue_depth"]) != 1 || byName["queue_depth"][0].Value != 7 {
		t.Fatalf("gauge row wrong: %+v", byName["queue_depth"])
	}
	if len(byName["some_untyped_metric"]) != 1 || byName["some_untyped_metric"][0].Type != metrics.TypeGauge {
		t.Fatalf("untyped row wrong: %+v", byName["some_untyped_metric"])
	}
	// histogram downcast: _sum/_count only, no bucket rows
	if len(byName["req_duration_seconds_sum"]) != 1 || byName["req_duration_seconds_sum"][0].Value != 9.5 {
		t.Fatalf("histogram sum wrong: %+v", byName["req_duration_seconds_sum"])
	}
	if len(byName["req_duration_seconds_count"]) != 1 || byName["req_duration_seconds_count"][0].Value != 120 {
		t.Fatalf("histogram count wrong: %+v", byName["req_duration_seconds_count"])
	}
	if len(byName["req_duration_seconds_bucket"]) != 0 {
		t.Fatal("bucket rows must be dropped")
	}
	for _, r := range rec.snapshot() {
		if r.Service != "shop" || r.Host != "h1" {
			t.Fatalf("attribution not stamped: %+v", r)
		}
	}
	one := byName["http_requests_total"][0]
	if !strings.Contains(one.Labels, `"code"`) {
		t.Fatalf("labels missing: %q", one.Labels)
	}
}

func TestScraperRecordsErrors(t *testing.T) {
	store, _, scraper := scrapeSetup(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	created := createAndGet(t, store, target(srv.URL))
	runScraper(t, scraper)
	after := waitScraped(t, store, created.ID)
	if !after.LastError.Valid || !strings.Contains(after.LastError.String, "HTTP 500") {
		t.Fatalf("expected HTTP 500 error recorded, got %+v", after.LastError)
	}
	if !after.LastErrorAt.Valid {
		t.Fatal("last_error_at not set")
	}
}

func TestScraperSaturationRecorded(t *testing.T) {
	store, rec, scraper := scrapeSetup(t)
	rec.reject = true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("queue_depth 7\n"))
	}))
	t.Cleanup(srv.Close)

	created := createAndGet(t, store, target(srv.URL))
	runScraper(t, scraper)
	after := waitScraped(t, store, created.ID)
	if !after.LastError.Valid || !strings.Contains(after.LastError.String, "saturated") {
		t.Fatalf("expected saturation error, got %+v", after.LastError)
	}
}

func TestScrapeTargetValidation(t *testing.T) {
	store, _, _ := scrapeSetup(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*services.ScrapeTarget)
	}{
		{"empty service", func(t *services.ScrapeTarget) { t.Service = "" }},
		{"empty host", func(t *services.ScrapeTarget) { t.Host = "" }},
		{"bad url", func(t *services.ScrapeTarget) { t.URL = "ftp://x" }},
		{"no scheme", func(t *services.ScrapeTarget) { t.URL = "localhost:9100/metrics" }},
		{"interval too short", func(t *services.ScrapeTarget) { t.ScrapeIntervalMs = 1000 }},
		{"interval too long", func(t *services.ScrapeTarget) { t.ScrapeIntervalMs = 4_000_000 }},
		{"auth header without colon", func(t *services.ScrapeTarget) { t.AuthHeader = "Bearer abc" }},
	}
	for _, tc := range cases {
		st := target("http://localhost:9100/metrics")
		tc.mutate(&st)
		if err := store.Create(ctx, st); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}

	if err := store.Create(ctx, target("http://localhost:9100/metrics")); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
}

func TestEnsureSelfScrapeIdempotent(t *testing.T) {
	store, _, _ := scrapeSetup(t)
	ctx := context.Background()

	if err := store.EnsureSelfScrape(ctx, "localhost:8080", "X-API-Key: first-secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSelfScrape(ctx, "localhost:8080", "X-API-Key: rotated-secret"); err != nil {
		t.Fatal(err)
	}
	targets, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected exactly 1 self target, got %d", len(targets))
	}
	if targets[0].Service != "pooml" || targets[0].Host != "self" {
		t.Fatalf("self target identity wrong: %+v", targets[0])
	}
	// second call refreshed the stored (encrypted) header: env rotation sticks
	if targets[0].AuthHeader != "X-API-Key: rotated-secret" {
		t.Fatalf("auth header not refreshed: %q", targets[0].AuthHeader)
	}
}
