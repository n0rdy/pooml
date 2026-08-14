package metrics_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/metrics"
)

func setup(t *testing.T) *db.Pools {
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

func countRows(t *testing.T, mdb *sql.DB) int {
	t.Helper()
	var n int
	if err := mdb.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitForRows(t *testing.T, mdb *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, mdb) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d rows, got %d", want, countRows(t, mdb))
}

func row(name string, v float64) metrics.Row {
	return metrics.Row{
		Timestamp: time.Now().UnixMilli(),
		Name:      name,
		Type:      metrics.TypeGauge,
		Value:     v,
		Service:   "svc",
		Host:      "h1",
	}
}

func TestPipelineWritesBatches(t *testing.T) {
	pools := setup(t)
	p := metrics.NewPipeline(pools.Metrics)
	p.Start()

	batch := []metrics.Row{
		{Timestamp: 1000, Name: "http_requests_total", Type: metrics.TypeCounter, Value: 42, Service: "api", Host: "h1", Labels: `{"status":"200"}`},
		{Timestamp: 2000, Name: "mem_bytes", Type: metrics.TypeGauge, Value: 1.5e9, Service: "api", Host: "h1"},
	}
	if !p.TryPush(batch) {
		t.Fatal("push rejected on empty pipeline")
	}
	waitForRows(t, pools.Metrics, 2)
	p.Shutdown()

	var name string
	var typ int
	var value float64
	var labels sql.NullString
	err := pools.Metrics.QueryRow(
		"SELECT name, type, value, labels FROM metrics WHERE name = 'http_requests_total'",
	).Scan(&name, &typ, &value, &labels)
	if err != nil {
		t.Fatal(err)
	}
	if typ != metrics.TypeCounter || value != 42 || !labels.Valid || labels.String != `{"status":"200"}` {
		t.Fatalf("unexpected row: type=%d value=%v labels=%v", typ, value, labels)
	}

	// empty labels must store NULL, not ""
	err = pools.Metrics.QueryRow("SELECT labels FROM metrics WHERE name = 'mem_bytes'").Scan(&labels)
	if err != nil {
		t.Fatal(err)
	}
	if labels.Valid {
		t.Fatalf("expected NULL labels, got %q", labels.String)
	}
}

func TestPipelineFlushesOversizedBatch(t *testing.T) {
	pools := setup(t)
	p := metrics.NewPipeline(pools.Metrics)
	p.Start()

	big := make([]metrics.Row, 1234)
	for i := range big {
		big[i] = row("bulk_metric", float64(i))
	}
	if !p.TryPush(big) {
		t.Fatal("push rejected")
	}
	p.Shutdown() // final flush must land everything
	if got := countRows(t, pools.Metrics); got != 1234 {
		t.Fatalf("expected 1234 rows after shutdown flush, got %d", got)
	}
}

func TestTryPushSaturation(t *testing.T) {
	pools := setup(t)
	p := metrics.NewPipeline(pools.Metrics)
	// writer deliberately not started: the channel alone must absorb its
	// capacity and then reject
	pushed := 0
	for p.TryPush([]metrics.Row{row("m", 1)}) {
		pushed++
		if pushed > 10000 {
			t.Fatal("TryPush never rejected")
		}
	}
	if pushed == 0 {
		t.Fatal("first push rejected")
	}
	if !p.TryPush(nil) {
		t.Fatal("empty batch must always succeed")
	}
}
