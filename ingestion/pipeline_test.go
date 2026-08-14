package ingestion_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0rdy/pooml/api"
	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/metrics"
	"github.com/n0rdy/pooml/services"
)

// testAPIKey is minted per-setup via the real ApiKeysService.
var testAPIKey string

func setup(t *testing.T) (*db.Pools, *ingestion.Pipeline, *ingestion.Broadcaster, *httptest.Server) {
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

	apiKeys := services.NewApiKeysService(pools.Meta)
	testAPIKey, err = apiKeys.Create(context.Background(), "integration-test")
	if err != nil {
		t.Fatal(err)
	}

	metricsPipeline := metrics.NewPipeline(pools.Metrics)
	metricsPipeline.Start()
	t.Cleanup(metricsPipeline.Shutdown)

	router := api.NewRouter(
		services.NewMonitoringService(pools),
		services.NewThrottlingService(),
		apiKeys, pipeline, metricsPipeline, "local", false,
	)
	srv := httptest.NewServer(router.NewRouter())
	t.Cleanup(srv.Close)

	return pools, pipeline, broadcaster, srv
}

func post(t *testing.T, srv *httptest.Server, path, body, apiKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func waitForCount(t *testing.T, dbc *sql.DB, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if err := dbc.QueryRow(query).Scan(&got); err == nil && got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("query %q: got %d rows, want %d", query, got, want)
}

func TestIngestEndToEnd(t *testing.T) {
	pools, pipeline, broadcaster, srv := setup(t)

	liveCh, unsub := broadcaster.Subscribe()
	defer unsub()

	payload := `{"level":"error","time":"2026-08-12T10:00:00Z","message":"db connection lost OutOfMemoryError"}
10.0.0.1 - - [12/Aug/2026:10:15:30 +0000] "GET /api/v1/users HTTP/1.1" 500 12 "-" "curl/8"
plain INFO all good`

	resp := post(t, srv, "/api/v1/ingest/payment-svc/hetzner-1", payload, testAPIKey)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	waitForCount(t, pools.LogsRead, "SELECT count(*) FROM logs", 3)

	var service, host string
	var level int
	err := pools.LogsRead.QueryRow(
		"SELECT service, host, level FROM logs WHERE message = 'GET /api/v1/users 500'").
		Scan(&service, &host, &level)
	if err != nil {
		t.Fatal(err)
	}
	if service != "payment-svc" || host != "hetzner-1" || level != 4 {
		t.Errorf("row = %s/%s/level=%d", service, host, level)
	}

	// FTS index populated via trigger, searchable through the read-only pool
	var ftsHits int
	if err := pools.LogsRead.QueryRow(
		"SELECT count(*) FROM logs_fts WHERE logs_fts MATCH 'OutOfMemoryError'").Scan(&ftsHits); err != nil {
		t.Fatal(err)
	}
	if ftsHits != 1 {
		t.Errorf("fts hits = %d, want 1", ftsHits)
	}

	// all three logs reached the broadcaster before persistence
	received := 0
	timeout := time.After(2 * time.Second)
	for received < 3 {
		select {
		case <-liveCh:
			received++
		case <-timeout:
			t.Fatalf("broadcaster delivered %d logs, want 3", received)
		}
	}

	pipeline.Shutdown()
}

func TestIngestAuthAndValidation(t *testing.T) {
	_, pipeline, _, srv := setup(t)
	defer pipeline.Shutdown()

	if resp := post(t, srv, "/api/v1/ingest/svc/host", "a log line", "wrong-key"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", resp.StatusCode)
	}
	if resp := post(t, srv, "/api/v1/ingest/svc/host", "a log line", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	if resp := post(t, srv, "/api/v1/ingest/svc/host", "   \n  ", testAPIKey); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank payload: status = %d, want 400", resp.StatusCode)
	}
	if resp := post(t, srv, "/api/v1/ingest/svc/host", strings.Repeat("x", 3<<20), testAPIKey); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized payload: status = %d, want 413", resp.StatusCode)
	}
	if resp := post(t, srv, "/api/v1/ingest/"+strings.Repeat("s", 201)+"/host", "line", testAPIKey); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized service param: status = %d, want 400", resp.StatusCode)
	}
}

// Shutdown must flush a partial batch that the 100ms timer hasn't fired for.
func TestShutdownFlushesPartialBatch(t *testing.T) {
	pools, pipeline, _, _ := setup(t)

	if !pipeline.TryIngest("flush-svc", "h", []byte("last words before shutdown"), time.Now().UnixMilli()) {
		t.Fatal("TryIngest refused")
	}
	pipeline.Shutdown()

	var cnt int
	if err := pools.LogsRead.QueryRow(
		"SELECT count(*) FROM logs WHERE service = 'flush-svc'").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("persisted %d rows after shutdown, want 1 (final flush)", cnt)
	}
}
