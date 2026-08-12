package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"
)

const (
	driverLogsRead  = "sqlite3-pooml-logs-read"
	driverLogsWrite = "sqlite3-pooml-logs-write"
	driverMetrics   = "sqlite3-pooml-metrics"
	driverMeta      = "sqlite3-pooml-meta"
)

// Connection-level pragmas, applied per-connection via ConnectHook.
// They do NOT survive across the Go connection pool unless applied here:
// db.Exec("PRAGMA ...") would only affect the one connection that ran it.
//
// cache_size is negative = KB (positive would mean pages).
// hard_heap_limit caps SQLite's per-connection heap so a runaway user query
// returns SQLITE_NOMEM instead of OOMing the process.
var (
	logsReadPragmas = []string{
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -10000",         // 10 MB per read conn
		"PRAGMA hard_heap_limit = 104857600", // 100 MB cap on user queries
	}

	logsWritePragmas = []string{
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -40000", // 40 MB on the single write conn
		"PRAGMA optimize = 0x10002",  // SQLite-recommended one-shot init
	}

	metricsPragmas = []string{
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -10000",
		"PRAGMA hard_heap_limit = 104857600",
		"PRAGMA optimize = 0x10002",
	}

	metaPragmas = []string{
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -2000", // 2 MB; meta is small
		"PRAGMA optimize = 0x10002",
		"PRAGMA foreign_keys = ON", // enforce ON DELETE CASCADE on alert_firings, dashboard_panels
	}

	registerOnce sync.Once
)

func registerDrivers() {
	registerOnce.Do(func() {
		sql.Register(driverLogsRead, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(logsReadPragmas)})
		sql.Register(driverLogsWrite, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(logsWritePragmas)})
		sql.Register(driverMetrics, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(metricsPragmas)})
		sql.Register(driverMeta, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(metaPragmas)})
	})
}

func makeConnectHook(pragmas []string) func(*sqlite3.SQLiteConn) error {
	return func(conn *sqlite3.SQLiteConn) error {
		for _, p := range pragmas {
			if _, err := conn.Exec(p, nil); err != nil {
				return fmt.Errorf("apply %q: %w", p, err)
			}
		}
		return nil
	}
}

// Pools owns the four SQLite connection pools used by pooml.
//
// LogsRead and LogsWrite point at logs.db with different access modes and
// pragma sets. The single-connection write pool serializes writes for WAL
// safety; the read pool serves user queries, alert evaluation, and filtered
// streaming polls concurrently.
//
// Metrics and Meta are read+write pools - much lower volume than logs.
type Pools struct {
	LogsRead  *sql.DB
	LogsWrite *sql.DB
	Metrics   *sql.DB
	Meta      *sql.DB
}

// OpenPools opens all four pools. Migrations must have run first so the
// database files exist (the read-only pool requires the file to be present).
func OpenPools(dbDir string) (*Pools, error) {
	registerDrivers()

	logsPath := filepath.Join(dbDir, logsDBFile)
	metricsPath := filepath.Join(dbDir, metricsDBFile)
	metaPath := filepath.Join(dbDir, metaDBFile)

	poolSize := runtime.NumCPU()*2 + 1

	logsRead, err := openPool(driverLogsRead, "file:"+logsPath+"?mode=ro", poolSize, poolSize)
	if err != nil {
		return nil, fmt.Errorf("logs read pool: %w", err)
	}

	logsWrite, err := openPool(driverLogsWrite, "file:"+logsPath+"?mode=rw", 1, 1)
	if err != nil {
		logsRead.Close()
		return nil, fmt.Errorf("logs write pool: %w", err)
	}

	metrics, err := openPool(driverMetrics, "file:"+metricsPath+"?mode=rw", poolSize, poolSize)
	if err != nil {
		logsRead.Close()
		logsWrite.Close()
		return nil, fmt.Errorf("metrics pool: %w", err)
	}

	meta, err := openPool(driverMeta, "file:"+metaPath+"?mode=rw", poolSize, poolSize)
	if err != nil {
		logsRead.Close()
		logsWrite.Close()
		metrics.Close()
		return nil, fmt.Errorf("meta pool: %w", err)
	}

	return &Pools{
		LogsRead:  logsRead,
		LogsWrite: logsWrite,
		Metrics:   metrics,
		Meta:      meta,
	}, nil
}

func openPool(driverName, dataSource string, maxOpen, maxIdle int) (*sql.DB, error) {
	db, err := sql.Open(driverName, dataSource)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

// Close shuts down all four pools, logging individual errors and continuing
// so a single misbehaving pool can't block the others.
func (p *Pools) Close() {
	closeOne := func(name string, db *sql.DB) {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Str("pool", name).Msg("close pool")
		}
	}
	closeOne("logs-read", p.LogsRead)
	closeOne("logs-write", p.LogsWrite)
	closeOne("metrics", p.Metrics)
	closeOne("meta", p.Meta)
}
