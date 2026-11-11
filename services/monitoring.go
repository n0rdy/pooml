package services

import (
	"context"
	"database/sql"

	"github.com/n0rdy/pooml/db"

	"github.com/rs/zerolog/log"
)

// MonitoringService answers liveness/readiness probes. It pings all four
// SQLite pools - if any is unreachable, pooml is unhealthy.
type MonitoringService struct {
	pools *db.Pools
}

func NewMonitoringService(pools *db.Pools) *MonitoringService {
	return &MonitoringService{pools: pools}
}

func (ms *MonitoringService) IsHealthy(ctx context.Context) bool {
	checks := []struct {
		name string
		db   *sql.DB
	}{
		// logs-write is deliberately absent: it is ONE connection, so a ping
		// waits behind any long flush or retention chunk and reports "busy"
		// as "down" - and a proxy that honours health then drops the UI too
		{"logs-read", ms.pools.LogsRead},
		{"metrics", ms.pools.Metrics},
		{"meta", ms.pools.Meta},
	}
	for _, c := range checks {
		if err := c.db.PingContext(ctx); err != nil {
			log.Error().Err(err).Str("pool", c.name).Msg("healthcheck ping failed")
			return false
		}
	}
	return true
}
