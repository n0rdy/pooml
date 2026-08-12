package db

import "embed"

//go:embed migrations/logs/*.sql
var logsMigrationsFS embed.FS

//go:embed migrations/metrics/*.sql
var metricsMigrationsFS embed.FS

//go:embed migrations/meta/*.sql
var metaMigrationsFS embed.FS
