package db

import (
	"testing"
	"time"
)

// End-to-end round-trip through the real migrations and pools: insert fires
// the FTS sync trigger, MATCH reads it back through the read-only pool.
// Guards against migration/runtime engine drift and a missing sqlite_fts5
// build tag - a plain build passes both, then dies on the first ingested log.
func TestMigrateAndPoolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	MigrateAll(dir)

	pools, err := OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	for pragma, want := range map[string]string{
		"auto_vacuum":     "2", // INCREMENTAL, persisted by the migration connection
		"journal_mode":    "wal",
		"hard_heap_limit": "1073741824",
	} {
		var got string
		if err := pools.LogsWrite.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}

	now := time.Now().UnixMilli()
	if _, err := pools.LogsWrite.Exec(
		`INSERT INTO logs(timestamp, ingested_at, level, service, host, message, parsed, raw)
		 VALUES (?, ?, 4, 'smoke-svc', 'smoke-host', 'smoke message', NULL, 'smoke raw OutOfMemoryError line')`,
		now, now); err != nil {
		t.Fatalf("insert through FTS trigger: %v", err)
	}

	var cnt int
	if err := pools.LogsRead.QueryRow(
		"SELECT count(*) FROM logs_fts WHERE logs_fts MATCH 'OutOfMemoryError'").Scan(&cnt); err != nil {
		t.Fatalf("FTS MATCH via read pool: %v", err)
	}
	if cnt != 1 {
		t.Errorf("FTS MATCH hits = %d, want 1", cnt)
	}

	// read-only enforcement is engine-level, not convention
	if _, err := pools.LogsRead.Exec("DELETE FROM logs"); err == nil {
		t.Error("write through the read-only pool succeeded; mode=ro not enforced")
	}
}

// The authorizer is defense in depth under the AST validation: even raw SQL
// reaching the read pool must not be able to run PRAGMA/ATTACH/load_extension.
func TestReadPoolAuthorizer(t *testing.T) {
	dir := t.TempDir()
	MigrateAll(dir)
	pools, err := OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	denied := []string{
		"PRAGMA cache_size",
		"ATTACH DATABASE 'x.db' AS x",
		"SELECT load_extension('evil')",
	}
	for _, q := range denied {
		if _, err := pools.LogsRead.Exec(q); err == nil {
			t.Errorf("%q succeeded on the read pool, want authorizer denial", q)
		}
	}

	// same ops stay legal on the write pool (jobs run PRAGMA optimize etc.)
	var v string
	if err := pools.LogsWrite.QueryRow("PRAGMA cache_size").Scan(&v); err != nil {
		t.Errorf("PRAGMA on write pool: %v", err)
	}

	// plain reads on the read pool are unaffected
	var cnt int
	if err := pools.LogsRead.QueryRow("SELECT count(*) FROM logs").Scan(&cnt); err != nil {
		t.Errorf("plain SELECT on read pool: %v", err)
	}
}
