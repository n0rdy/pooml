package db

import (
	"embed"
	"errors"
	"fmt"
	"path/filepath"

	_ "github.com/golang-migrate/migrate/v4/database/sqlite"

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
	// x-no-tx-wrap=true disables migrate's automatic transaction wrapping -
	// required because PRAGMAs (and the FTS5 virtual-table CREATE) don't play
	// well inside a wrapped transaction.
	// See: https://github.com/golang-migrate/migrate/issues/346
	//
	// _pragma=auto_vacuum(INCREMENTAL) is honored by modernc.org/sqlite (the
	// driver behind migrate's "sqlite" registration). It runs on connection
	// open, BEFORE migrate creates its schema_migrations bookkeeping table -
	// which is the only point at which auto_vacuum can still be persisted in
	// the DB header. Once any table exists, auto_vacuum is silently ignored.
	// journal_mode is set in 000001_init.up.sql (Forq-style) since it can be
	// applied at any time.
	dbURL := fmt.Sprintf(
		"sqlite://file:%s?cache=shared&mode=rwc&x-no-tx-wrap=true&_pragma=auto_vacuum(INCREMENTAL)",
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
