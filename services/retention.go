package services

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
)

// Retention settings keys (plain). Values are day counts as strings.
const (
	SettingRetentionLogsDays    = "retention.logs_days"
	SettingRetentionMetricsDays = "retention.metrics_days"
	SettingRetentionFiringsDays = "retention.alert_firings_days"
)

const (
	RetentionDefaultLogsDays    = 30
	RetentionDefaultMetricsDays = 90
	RetentionDefaultFiringsDays = 90
	RetentionMinDays            = 1
	RetentionMaxDays            = 3650

	// hourly, not daily: 1/24 the per-sweep batch = 1/24 the lock contention
	// spike, and disk usage stays even. See CONTEXT.md > Data Retention.
	retentionSweepInterval = time.Hour

	// per-transaction delete cap: keeps each write-lock hold under ~100ms so
	// ingestion interleaves between chunks instead of stalling
	retentionChunk = 10000
)

type RetentionService struct {
	settings  *SettingsService
	logsWrite *sql.DB
	metricsDB *sql.DB
	metaDB    *sql.DB
}

func NewRetentionService(settings *SettingsService, logsWrite, metricsDB, metaDB *sql.DB) *RetentionService {
	return &RetentionService{settings: settings, logsWrite: logsWrite, metricsDB: metricsDB, metaDB: metaDB}
}

func (rs *RetentionService) Run(ctx context.Context) {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rs.Sweep(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Sweep deletes expired rows from all retained tables and reclaims freed
// pages. Aborts cleanly mid-batch on ctx cancellation (partial cleanup is
// fine - the next sweep continues where this one stopped).
func (rs *RetentionService) Sweep(ctx context.Context) {
	now := time.Now()
	targets := []struct {
		name     string
		db       *sql.DB
		tsColumn string
		days     int
	}{
		{"logs", rs.logsWrite, "timestamp", rs.days(ctx, SettingRetentionLogsDays, RetentionDefaultLogsDays)},
		{"metrics", rs.metricsDB, "timestamp", rs.days(ctx, SettingRetentionMetricsDays, RetentionDefaultMetricsDays)},
		{"alert_firings", rs.metaDB, "fired_at", rs.days(ctx, SettingRetentionFiringsDays, RetentionDefaultFiringsDays)},
	}

	for _, t := range targets {
		threshold := now.Add(-time.Duration(t.days) * 24 * time.Hour).UnixMilli()
		deleted, err := deleteExpired(ctx, t.db, t.name, t.tsColumn, threshold)
		if err != nil {
			log.Error().Err(err).Str("table", t.name).Msg("retention sweep failed")
			continue
		}
		if deleted > 0 {
			RetentionDeleted.WithLabelValues(t.name).Add(float64(deleted))
			log.Info().Str("table", t.name).Int64("deleted", deleted).Int("retention_days", t.days).Msg("retention sweep")
		}
	}

	// reclaim freed pages in bounded chunks: a bare incremental_vacuum moves
	// the ENTIRE freelist in one transaction - after a big sweep that is a
	// multi-second write-lock hold, undoing the chunked deletes above.
	// meta.db churn is tiny and skips the vacuum.
	for _, d := range []struct {
		name string
		db   *sql.DB
	}{{"logs", rs.logsWrite}, {"metrics", rs.metricsDB}} {
		for ctx.Err() == nil {
			var before, after int
			if err := d.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&before); err != nil || before == 0 {
				break
			}
			if _, err := d.db.ExecContext(ctx, "PRAGMA incremental_vacuum(1000)"); err != nil {
				log.Error().Err(err).Str("db", d.name).Msg("incremental_vacuum failed")
				break
			}
			if err := d.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil || after >= before {
				break // no progress; don't spin
			}
		}
	}
}

// deleteExpired removes rows older than threshold in chunked transactions,
// releasing the write lock between chunks.
func deleteExpired(ctx context.Context, db *sql.DB, table, tsColumn string, threshold int64) (int64, error) {
	// table and tsColumn come from the fixed list above, never user input
	q := "DELETE FROM " + table + " WHERE id IN (SELECT id FROM " + table +
		" WHERE " + tsColumn + " < ? ORDER BY id LIMIT " + strconv.Itoa(retentionChunk) + ")"
	var total int64
	for {
		if ctx.Err() != nil {
			return total, nil
		}
		res, err := db.ExecContext(ctx, q, threshold)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < retentionChunk {
			return total, nil
		}
	}
}

func (rs *RetentionService) days(ctx context.Context, key string, def int) int {
	return RetentionDays(ctx, rs.settings, key, def)
}

// RetentionDays reads a retention setting, falling back to def when unset or
// out of bounds - an invalid row must never cause an accidental mass delete.
func RetentionDays(ctx context.Context, settings *SettingsService, key string, def int) int {
	v, err := settings.Get(ctx, key)
	if err != nil || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < RetentionMinDays || n > RetentionMaxDays {
		return def
	}
	return n
}

// RunOptimizer refreshes the query planner's statistics hourly on every
// write handle. Cheap: SQLite only re-analyzes tables whose content changed
// enough to matter.
func RunOptimizer(ctx context.Context, dbs map[string]*sql.DB) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			for name, d := range dbs {
				if _, err := d.ExecContext(ctx, "PRAGMA optimize"); err != nil {
					log.Error().Err(err).Str("db", name).Msg("PRAGMA optimize failed")
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
