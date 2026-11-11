package ingestion

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/db"

	"github.com/mattn/go-sqlite3"
)

// no Start(): workers must not drain, so admission control alone decides
func TestIngestByteBudget(t *testing.T) {
	p := NewPipeline(nil, NewBroadcaster())
	payload := make([]byte, 2<<20) // the max request size

	accepted := 0
	for range 1000 { // entry-count cap alone would admit all of these
		if !p.TryIngest("svc", "h", payload, 1) {
			break
		}
		accepted++
	}
	// each payload is charged for what it can grow to once parsed
	want := maxBufferedBytes / (admissionGrowth * (2 << 20))
	if accepted != want {
		t.Errorf("accepted %d payloads of 2 MiB, want %d (byte budget)", accepted, want)
	}

	// draining one event frees its bytes and admission resumes
	ev := <-p.ingestChan
	p.bufferedBytes.Add(-admissionGrowth * int64(len(ev.payload)))
	if !p.TryIngest("svc", "h", payload, 1) {
		t.Error("budget not released after drain")
	}
}

// The budget used to be released at parse time, leaving the parsed queue and
// the writer's batch unbounded: large lines piled up until SQLite ran out of heap.
func TestByteBudgetHeldUntilWritten(t *testing.T) {
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	p := NewPipeline(pools.LogsWrite, NewBroadcaster())
	// parser and fan-out only: with no writer, parsed logs must keep their bytes
	p.parserWG.Add(1)
	go p.parserWorker()
	p.fanoutWG.Add(1)
	go p.fanout()

	const lines, lineLen = 3, 64 << 10
	line := bytes.Repeat([]byte("x"), lineLen)
	for range lines {
		if !p.TryIngest("svc", "h", line, 1) {
			t.Fatal("ingest rejected")
		}
	}
	waitFor(t, "logs parsed", func() bool { return len(p.batchInputChan) == lines })
	if got := p.bufferedBytes.Load(); got < lines*lineLen {
		t.Fatalf("budget holds %d bytes for %d parsed lines, want at least %d", got, lines, lines*lineLen)
	}

	p.writerWG.Add(1)
	go p.batchWriter()
	waitFor(t, "budget released after the write", func() bool { return p.bufferedBytes.Load() == 0 })
	var n int
	if err := pools.LogsRead.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil || n != lines {
		t.Fatalf("stored %d rows (err %v), want %d", n, err, lines)
	}
	p.Shutdown()
}

// A row the database rejects must cost only itself, and its bytes must leave
// the budget, or ingestion stalls behind it.
func TestWriterSurvivesPoisonRow(t *testing.T) {
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	if _, err := pools.LogsWrite.Exec(`CREATE TRIGGER poison BEFORE INSERT ON logs
		WHEN new.raw = 'poison' BEGIN SELECT RAISE(ABORT, 'poison row'); END`); err != nil {
		t.Fatal(err)
	}

	p := NewPipeline(pools.LogsWrite, NewBroadcaster())
	p.Start()
	if !p.TryIngest("svc", "h", []byte("before\npoison\nafter\n"), 1) {
		t.Fatal("ingest rejected")
	}
	waitFor(t, "budget released", func() bool { return p.bufferedBytes.Load() == 0 })
	p.Shutdown()

	var n int
	if err := pools.LogsRead.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil || n != 2 {
		t.Fatalf("stored %d rows (err %v), want the 2 good ones", n, err)
	}
}

// Batches must close on bytes, not only on row count. Each flush is one
// INSERT, so commits on the writer's connection count the batches.
func TestBatchClosesOnBytes(t *testing.T) {
	big := string(bytes.Repeat([]byte("x"), 1<<20))
	if got, want := logBytes(&common.StandardLog{Raw: big, Message: &big, Parsed: &big}), int64(3<<20); got != want {
		t.Fatalf("logBytes = %d, want %d (each bound column counts)", got, want)
	}

	old := maxBatchBytes
	maxBatchBytes = 64 << 10
	t.Cleanup(func() { maxBatchBytes = old })

	dir := t.TempDir()
	db.MigrateAll(dir)
	var commits atomic.Int64
	driver := fmt.Sprintf("sqlite3-commit-counter-%d", time.Now().UnixNano())
	sql.Register(driver, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		c.RegisterCommitHook(func() int { commits.Add(1); return 0 })
		return nil
	}})
	writeDB, err := sql.Open(driver, "file:"+filepath.Join(dir, "logs.db")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writeDB.Close() })
	writeDB.SetMaxOpenConns(1)

	// 12 rows of 20 KiB, far under the 1000-row cap: only bytes can split them
	p := NewPipeline(writeDB, NewBroadcaster())
	level := common.LevelInfo
	row := common.StandardLog{Timestamp: 1, IngestedAt: 1, Level: &level, Service: "s", Host: "h", Raw: string(bytes.Repeat([]byte("y"), 20<<10))}
	const rows = 12
	for range rows {
		p.bufferedBytes.Add(logBytes(&row))
		p.batchInputChan <- row
	}
	close(p.batchInputChan)
	p.writerWG.Add(1)
	go p.batchWriter()
	p.writerWG.Wait()

	var n int
	if err := writeDB.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil || n != rows {
		t.Fatalf("stored %d rows (err %v), want %d", n, err, rows)
	}
	// 4 rows reach the 64 KiB threshold, so 12 rows are three batches
	if got := commits.Load(); got != 3 {
		t.Fatalf("%d rows of 20 KiB landed in %d commit(s), want 3 byte-bounded batches", rows, got)
	}
	if got := p.bufferedBytes.Load(); got != 0 {
		t.Fatalf("budget still holds %d bytes after the writes", got)
	}
}

// Decoding can grow a log far past the 2 MiB payload cap, and the read pool
// refuses a row over its limit as soon as a query sorts it: one such log used
// to make every search matching it fail with "string or blob too big".
func TestStoredRowsStaySearchable(t *testing.T) {
	big := strings.Repeat("z", maxLogRowBytes)
	msg := "m"
	for name, l := range map[string]common.StandardLog{
		"parsed dropped first": {Raw: big[:maxLogRowBytes/2+1], Parsed: &big, Message: &msg},
		"then message":         {Raw: big[:maxLogRowBytes-1], Parsed: &big, Message: &big},
		"then raw truncated":   {Raw: big + "tail é", Parsed: &big, Message: &big},
	} {
		clampLog(&l)
		if got := logBytes(&l); got > maxLogRowBytes {
			t.Errorf("%s: %d bytes after clamping, want at most %d", name, got, maxLogRowBytes)
		}
		if l.Raw == "" || !utf8.ValidString(l.Raw) {
			t.Errorf("%s: raw must survive as valid text", name)
		}
	}
	small := common.StandardLog{Raw: "line", Parsed: &msg, Message: &msg}
	clampLog(&small)
	if small.Parsed == nil || small.Message == nil {
		t.Error("ordinary logs must be left alone")
	}

	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	p := NewPipeline(pools.LogsWrite, NewBroadcaster())
	p.Start()

	// a FluentBit record with no "log" field is re-encoded: '<' used to become
	// six bytes, and invalid UTF-8 still decodes to three per byte
	filler := (2<<20 - 200) / 2
	payload := `[{"date":1,"note":"needle error","x":"` + strings.Repeat("<", filler) + strings.Repeat("\xff", filler) + `"}]`
	if !p.TryIngest("svc", "h", []byte(payload), time.Now().UnixMilli()) {
		t.Fatal("ingest rejected")
	}
	waitFor(t, "row written", func() bool { return p.bufferedBytes.Load() == 0 })
	p.Shutdown()

	var rowBytes int64
	if err := pools.LogsWrite.QueryRow(
		"SELECT length(CAST(raw AS BLOB)) + COALESCE(length(CAST(parsed AS BLOB)), 0) + COALESCE(length(CAST(message AS BLOB)), 0) FROM logs").Scan(&rowBytes); err != nil {
		t.Fatal(err)
	}
	if rowBytes > maxLogRowBytes {
		t.Fatalf("stored row is %d bytes, want at most %d", rowBytes, maxLogRowBytes)
	}
	// the shapes the UI actually runs: FTS combined with a sort, and a sorted SELECT *
	for _, q := range []string{
		`SELECT logs.id FROM logs JOIN logs_fts ON logs.id = logs_fts."rowid" WHERE logs_fts.raw MATCH '"needle"*' ORDER BY timestamp DESC LIMIT 100`,
		`SELECT id FROM (SELECT * FROM logs ORDER BY length(service)) LIMIT 10`,
	} {
		var id int64
		if err := pools.LogsRead.QueryRow(q).Scan(&id); err != nil {
			t.Errorf("read pool query failed on the oversized row: %v\n%s", err, q)
		}
	}
}

// A writer that keeps failing answers pings and returns 204 to shippers until
// the queues fill: its own state is the only thing that can report it.
func TestWriterReportsItsHealth(t *testing.T) {
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	p := NewPipeline(pools.LogsWrite, NewBroadcaster())
	p.Start()
	if !p.TryIngest("svc", "h", []byte("a line"), 1) {
		t.Fatal("ingest rejected")
	}
	waitFor(t, "first write", func() bool { return p.bufferedBytes.Load() == 0 })
	if since, msg := p.Health.Failing(); since != 0 || msg != "" {
		t.Fatalf("healthy writer reports failing: since=%d msg=%q", since, msg)
	}

	// every flush fails from here on, and not because of the row itself
	pools.LogsWrite.Close()
	before := time.Now().UnixMilli()
	if !p.TryIngest("svc", "h", []byte("another line"), 1) {
		t.Fatal("ingest rejected")
	}
	waitFor(t, "writer reports the failure", func() bool { since, _ := p.Health.Failing(); return since != 0 })
	first, msg := p.Health.Failing()
	if first < before || msg == "" {
		t.Fatalf("since=%d (test began %d) msg=%q", first, before, msg)
	}
	// the streak keeps its first failure time across retries
	time.Sleep(3 * batchTimeout)
	if since, _ := p.Health.Failing(); since != first {
		t.Fatalf("failing-since moved from %d to %d while still failing", first, since)
	}
	p.Shutdown() // or the writer keeps retrying (and logging) for the rest of the package's tests
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
