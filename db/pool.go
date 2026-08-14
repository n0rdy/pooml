package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

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
var (
	logsReadPragmas = []string{
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -10000", // 10 MB per read conn
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

	// The logs-read hook bakes in the metrics.db path for ATTACH, so its
	// driver is registered per OpenPools call under a unique name. A shared
	// package var instead would let concurrently-open pools (tests) attach
	// each other's files.
	logsReadDriverSeq atomic.Int64
)

func registerDrivers() {
	registerOnce.Do(func() {
		sql.Register(driverLogsWrite, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(logsWritePragmas)})
		sql.Register(driverMetrics, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(metricsPragmas)})
		sql.Register(driverMeta, &sqlite3.SQLiteDriver{ConnectHook: makeConnectHook(metaPragmas)})
	})
}

func registerLogsReadDriver(metricsPath string) string {
	name := fmt.Sprintf("%s-%d", driverLogsRead, logsReadDriverSeq.Add(1))
	sql.Register(name, &sqlite3.SQLiteDriver{ConnectHook: makeLogsReadHook(metricsPath)})
	return name
}

// makeLogsReadHook: pragmas, then ATTACH metrics.db read-only (so user
// queries can JOIN logs and metrics in one statement), then the authorizer:
// defense in depth under the AST validation. Order matters - the authorizer
// would deny our own pragmas and the ATTACH. Registered only on the read
// pool; jobs and ingestion on the other pools legitimately run PRAGMA
// (optimize, incremental_vacuum).
func makeLogsReadHook(metricsPath string) func(*sqlite3.SQLiteConn) error {
	return func(conn *sqlite3.SQLiteConn) error {
		if err := makeConnectHook(logsReadPragmas)(conn); err != nil {
			return err
		}
		attach := fmt.Sprintf("ATTACH DATABASE 'file:%s?mode=ro' AS metricsdb",
			strings.ReplaceAll(metricsPath, "'", "''"))
		if _, err := conn.Exec(attach, nil); err != nil {
			return fmt.Errorf("attach metrics.db: %w", err)
		}
		conn.RegisterAuthorizer(readAuthorizer)
		return nil
	}
}

func readAuthorizer(op int, arg1, arg2, arg3 string) int {
	switch op {
	case sqlite3.SQLITE_PRAGMA:
		// FTS5 runs `PRAGMA data_version` internally on every MATCH to detect
		// external changes; denying it breaks FTS queries entirely
		if strings.EqualFold(arg1, "data_version") {
			return sqlite3.SQLITE_OK
		}
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_DETACH:
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_FUNCTION:
		if strings.EqualFold(arg2, "load_extension") {
			return sqlite3.SQLITE_DENY
		}
	}
	return sqlite3.SQLITE_OK
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

	logsRead, err := openPool(registerLogsReadDriver(metricsPath), "file:"+logsPath+"?mode=ro", poolSize, poolSize)
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

	// hard_heap_limit is process-global (all pools share it) and cannot be
	// lowered or cleared once set, so it is a one-shot OOM backstop here, not
	// a per-pool query cap. See CONTEXT.md > Performance > SQLite Configuration.
	if _, err := meta.Exec("PRAGMA hard_heap_limit = 1073741824"); err != nil {
		logsRead.Close()
		logsWrite.Close()
		metrics.Close()
		meta.Close()
		return nil, fmt.Errorf("set hard_heap_limit: %w", err)
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
