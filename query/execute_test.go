package query_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/query"
)

func setupDB(t *testing.T) *db.Pools {
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

func insertRows(t *testing.T, pools *db.Pools, n int, service, rawPrefix string) {
	t.Helper()
	const perStmt = 4000 // 5 params each, safely under SQLITE_LIMIT_VARIABLE_NUMBER
	now := time.Now().UnixMilli()
	for off := 0; off < n; off += perStmt {
		cnt := min(perStmt, n-off)
		var sb strings.Builder
		sb.WriteString("INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES ")
		args := make([]any, 0, cnt*6)
		for i := range cnt {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,?,?,?,?)")
			args = append(args, now+int64(off+i), now, 2, service, "test-host", fmt.Sprintf("%s line %d", rawPrefix, off+i))
		}
		if _, err := pools.LogsWrite.Exec(sb.String(), args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecuteBasic(t *testing.T) {
	pools := setupDB(t)
	insertRows(t, pools, 20, "payment-svc", "hello")
	insertRows(t, pools, 5, "auth-svc", "special OutOfMemoryError")

	v, err := query.Validate("SELECT service, raw FROM logs WHERE service = 'payment-svc' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	res, err := query.Execute(context.Background(), pools.LogsRead, v.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "service" {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 20 || res.Truncated {
		t.Errorf("rows = %d, truncated = %v", len(res.Rows), res.Truncated)
	}
	if s, ok := res.Rows[0][1].(string); !ok || !strings.HasPrefix(s, "hello line") {
		t.Errorf("raw cell = %#v, want string", res.Rows[0][1])
	}
}

func TestExecuteFTSCombined(t *testing.T) {
	pools := setupDB(t)
	insertRows(t, pools, 20, "payment-svc", "hello")
	insertRows(t, pools, 5, "auth-svc", "special OutOfMemoryError")

	v, err := query.Validate("SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CombineFTS("and"); err != nil {
		t.Fatal(err)
	}
	res, err := query.Execute(context.Background(), pools.LogsRead, v.SQL(), "OutOfMemoryError")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 {
		t.Errorf("fts rows = %d, want 5", len(res.Rows))
	}

	// OR must EXECUTE too: SQLite rejects `MATCH ... OR ...` outright
	// ("unable to use function MATCH in the requested context"), so the OR
	// path combines via an IN-subquery. Latent since M5, caught by the panel
	// parity tests.
	v, err = query.Validate("SELECT * FROM logs WHERE service = 'payment-svc' ORDER BY timestamp DESC LIMIT 100")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CombineFTS("or"); err != nil {
		t.Fatal(err)
	}
	res, err = query.Execute(context.Background(), pools.LogsRead, v.SQL(), "OutOfMemoryError")
	if err != nil {
		t.Fatalf("OR combine failed to execute: %v", err)
	}
	if len(res.Rows) != 25 { // 20 payment-svc + 5 fts matches
		t.Errorf("OR rows = %d, want 25", len(res.Rows))
	}
}

func TestExecuteScanCapCatchesNonLiteralLimit(t *testing.T) {
	pools := setupDB(t)
	insertRows(t, pools, query.MaxRows+1, "bulk-svc", "bulk")

	// a non-literal LIMIT slips past clampLimit by design; the scan cap is the
	// second line of defense
	v, err := query.Validate("SELECT id FROM logs LIMIT 10000 + 1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := query.Execute(context.Background(), pools.LogsRead, v.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != query.MaxRows || !res.Truncated {
		t.Errorf("rows = %d, truncated = %v; want %d, true", len(res.Rows), res.Truncated, query.MaxRows)
	}
}

func TestExecuteTimeout(t *testing.T) {
	pools := setupDB(t)
	insertRows(t, pools, query.MaxRows+1, "bulk-svc", "bulk")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// three-way cross join of 10k rows: ~1e12 candidate rows, cannot finish
	start := time.Now()
	_, err := query.Execute(ctx, pools.LogsRead,
		"SELECT count(*) FROM logs a JOIN logs b JOIN logs c")
	if err == nil {
		t.Fatal("runaway query completed, want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("query aborted after %v, interrupt not working", elapsed)
	}
}
