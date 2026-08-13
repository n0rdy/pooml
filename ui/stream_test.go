package ui_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/n0rdy/pooml/ui"
)

// sseCollect connects to an SSE path, runs trigger, and returns everything
// received until the deadline.
func sseCollect(cl *client, path string, dur time.Duration, trigger func()) string {
	cl.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cl.srv.URL+path, nil)
	if err != nil {
		cl.t.Fatal(err)
	}
	resp, err := cl.c.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cl.t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		cl.t.Fatalf("stream content type = %q", ct)
	}

	if trigger != nil {
		go func() {
			time.Sleep(150 * time.Millisecond) // let the strategy anchor first
			trigger()
		}()
	}

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestStreamBroadcastUnfiltered(t *testing.T) {
	restore := ui.SetStreamIntervalsForTest(100*time.Millisecond, 200*time.Millisecond)
	defer restore()
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	got := sseCollect(cl, "/logs/stream", 1200*time.Millisecond, func() {
		cl.pipeline.TryIngest("live-svc", "h", []byte(`{"level":"warn","msg":"live hello from broadcast"}`), time.Now().UnixMilli())
	})
	if !strings.Contains(got, "event: log") || !strings.Contains(got, "live hello from broadcast") {
		t.Errorf("broadcast stream missing row, got: %.400s", got)
	}
	if !strings.Contains(got, "live-svc") {
		t.Error("streamed row missing service badge")
	}
}

func TestStreamFilteredPolling(t *testing.T) {
	restore := ui.SetStreamIntervalsForTest(100*time.Millisecond, 200*time.Millisecond)
	defer restore()
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 2) // pre-existing rows must NOT stream (no backfill)

	q := url.QueryEscape("SELECT * FROM logs WHERE service = 'target-svc' ORDER BY timestamp DESC LIMIT 100")
	got := sseCollect(cl, "/logs/stream?q="+q, 1500*time.Millisecond, func() {
		cl.pipeline.TryIngest("target-svc", "h", []byte(`{"level":"error","msg":"filtered live row"}`), time.Now().UnixMilli())
		cl.pipeline.TryIngest("other-svc", "h", []byte(`{"level":"info","msg":"must not appear"}`), time.Now().UnixMilli())
	})
	if !strings.Contains(got, "filtered live row") {
		t.Errorf("matching row not streamed: %.400s", got)
	}
	if strings.Contains(got, "must not appear") {
		t.Error("non-matching row leaked into filtered stream")
	}
	if strings.Contains(got, "seeded line") {
		t.Error("backfill leaked: pre-existing rows must not stream")
	}
}

func TestStreamAggregationRefresh(t *testing.T) {
	restore := ui.SetStreamIntervalsForTest(100*time.Millisecond, 300*time.Millisecond)
	defer restore()
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())
	seedLogs(cl, 4)

	q := url.QueryEscape("SELECT service, count(*) AS cnt FROM logs GROUP BY service")
	got := sseCollect(cl, "/logs/stream?q="+q, 900*time.Millisecond, nil)
	if !strings.Contains(got, "event: refresh") {
		t.Fatalf("no refresh event: %.300s", got)
	}
	if !strings.Contains(got, "cnt") || !strings.Contains(got, "payment-svc") {
		t.Error("refresh payload missing aggregation table")
	}
}

func TestStreamRejectsInvalidQuery(t *testing.T) {
	cl := newClient(t)
	cl.login(testSecret, cl.csrfToken())

	req, _ := http.NewRequest(http.MethodGet, cl.srv.URL+"/logs/stream?q=DROP+TABLE+logs", nil)
	resp, err := cl.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid stream q = %d, want 400", resp.StatusCode)
	}
}
