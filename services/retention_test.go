package services_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/services"
)

func setupRetention(t *testing.T) (*services.RetentionService, *db.Pools, *services.SettingsService) {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	enc, err := services.NewEncryptionService("0123456789abcdef0123456789abcdef", pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	settings := services.NewSettingsService(pools.Meta, enc)
	return services.NewRetentionService(settings, pools.LogsWrite, pools.Metrics, pools.Meta), pools, settings
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("exec %.60s: %v", q, err)
	}
}

func queryCount(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionSweep(t *testing.T) {
	rs, pools, settings := setupRetention(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	dayMs := int64(24 * time.Hour / time.Millisecond)

	// logs: 5 expired (40 days old, default 30), 3 fresh
	for i := range 5 {
		mustExec(t, pools.LogsWrite, "INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (?,?,2,'svc','h','old')", now-40*dayMs-int64(i), now)
	}
	for i := range 3 {
		mustExec(t, pools.LogsWrite, "INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (?,?,2,'svc','h','new')", now-int64(i), now)
	}
	// metrics: 4 expired (100 days old, default 90), 2 fresh
	for i := range 4 {
		mustExec(t, pools.Metrics, "INSERT INTO metrics(timestamp, name, type, value, service, host) VALUES (?,'m',1,1.0,'svc','h')", now-100*dayMs-int64(i))
	}
	for i := range 2 {
		mustExec(t, pools.Metrics, "INSERT INTO metrics(timestamp, name, type, value, service, host) VALUES (?,'m',1,1.0,'svc','h')", now-int64(i))
	}
	// alert_firings: needs a parent alert; 2 expired, 1 fresh
	mustExec(t, pools.Meta, "INSERT INTO alerts(id, name, query, check_interval_ms, cooldown_ms, target, enabled, created_at) VALUES (1,'a','SELECT 1',60000,60000,'{}',1,?)", now)
	for i := range 2 {
		mustExec(t, pools.Meta, "INSERT INTO alert_firings(alert_id, fired_at, matched_rows) VALUES (1, ?, '[]')", now-100*dayMs-int64(i))
	}
	mustExec(t, pools.Meta, "INSERT INTO alert_firings(alert_id, fired_at, matched_rows) VALUES (1, ?, '[]')", now)

	rs.Sweep(ctx)

	if n := queryCount(t, pools.LogsWrite, "logs"); n != 3 {
		t.Errorf("logs after sweep = %d, want 3", n)
	}
	if n := queryCount(t, pools.Metrics, "metrics"); n != 2 {
		t.Errorf("metrics after sweep = %d, want 2", n)
	}
	if n := queryCount(t, pools.Meta, "alert_firings"); n != 1 {
		t.Errorf("alert_firings after sweep = %d, want 1", n)
	}

	// configured retention overrides the default: shrink logs to 1 day
	if err := settings.Set(ctx, services.SettingRetentionLogsDays, "1", false); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pools.LogsWrite, "INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (?,?,2,'svc','h','2d old')", now-2*dayMs, now)
	rs.Sweep(ctx)
	if n := queryCount(t, pools.LogsWrite, "logs"); n != 3 {
		t.Errorf("logs after 1-day sweep = %d, want 3 (fresh only)", n)
	}

	// out-of-range setting falls back to the default (no accidental wipe)
	if err := settings.Set(ctx, services.SettingRetentionLogsDays, "0", false); err != nil {
		t.Fatal(err)
	}
	rs.Sweep(ctx)
	if n := queryCount(t, pools.LogsWrite, "logs"); n != 3 {
		t.Errorf("logs after invalid-setting sweep = %d, want 3", n)
	}
}

func TestRetentionChunking(t *testing.T) {
	rs, pools, _ := setupRetention(t)
	now := time.Now().UnixMilli()
	old := now - 40*int64(24*time.Hour/time.Millisecond)

	// > one chunk of expired rows, multi-row inserts to keep setup fast
	const total = 25000
	const per = 5000
	for off := 0; off < total; off += per {
		var sb strings.Builder
		sb.WriteString("INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES ")
		args := make([]any, 0, per*2)
		for i := range per {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,2,'svc','h','bulk')")
			args = append(args, old-int64(off+i), now)
		}
		mustExec(t, pools.LogsWrite, sb.String(), args...)
	}

	rs.Sweep(context.Background())
	if n := queryCount(t, pools.LogsWrite, "logs"); n != 0 {
		t.Errorf("logs after chunked sweep = %d, want 0", n)
	}
	// FTS stays consistent with the content table via the content-link triggers
	var ftsRows int
	if err := pools.LogsWrite.QueryRow("SELECT COUNT(*) FROM logs_fts").Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows != 0 {
		t.Errorf("logs_fts after sweep = %d, want 0", ftsRows)
	}
}
