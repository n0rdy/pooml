package db

import (
	"embed"
	"errors"
	"fmt"
	"path/filepath"

	_ "github.com/golang-migrate/migrate/v4/database/sqlite3"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/rs/zerolog/log"
)

const (
	logsDBFile    = "logs.db"
	metricsDBFile = "metrics.db"
	metaDBFile    = "meta.db"
)

// MigrateAll runs the initial (and any subsequent) migrations against all
// three SQLite databases. Files are created on first run via mode=rwc.
// Each DB tracks its own schema_migrations table independently.
//
// Must run before OpenPools so the read-only pool has a file to open.
func MigrateAll(dbDir string) {
	migrateOne("logs", filepath.Join(dbDir, logsDBFile), logsMigrationsFS, "migrations/logs")
	migrateOne("metrics", filepath.Join(dbDir, metricsDBFile), metricsMigrationsFS, "migrations/metrics")
	migrateOne("meta", filepath.Join(dbDir, metaDBFile), metaMigrationsFS, "migrations/meta")
}

func migrateOne(name, dbPath string, migrationsFS embed.FS, fsRoot string) {
	// migrate's "sqlite3" registration is mattn-backed, same engine as the
	// runtime pools. The "sqlite" (modernc) one would silently give migrations
	// FTS5 while the mattn runtime lacks it without -tags sqlite_fts5.
	//
	// x-no-tx-wrap: PRAGMAs and the FTS5 virtual-table CREATE don't work inside
	// migrate's tx wrapping (github.com/golang-migrate/migrate/issues/346).
	// _auto_vacuum runs on connection open, before schema_migrations exists -
	// the only point where auto_vacuum can still be persisted in the DB header.
	dbURL := fmt.Sprintf(
		"sqlite3://file:%s?mode=rwc&x-no-tx-wrap=true&_auto_vacuum=incremental",
		dbPath,
	)

	source, err := iofs.New(migrationsFS, fsRoot)
	if err != nil {
		log.Fatal().Err(err).Str("db", name).Msg("failed to create migrations source")
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dbURL)
	if err != nil {
		log.Fatal().Err(err).Str("db", name).Msg("failed to create migration instance")
	}

	err = m.Up()
	switch {
	case err == nil:
		log.Info().Str("db", name).Msg("migrations applied")
	case errors.Is(err, migrate.ErrNoChange):
		log.Info().Str("db", name).Msg("migrations up to date")
	default:
		log.Fatal().Err(err).Str("db", name).Msg("failed to run migrations")
	}
}
