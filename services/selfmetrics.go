package services

import (
	"os"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PromRegistry backs GET /metrics; see CONTEXT.md > Internal Metrics.
// Own registry, not the client_golang default: nothing sneaks in unreviewed.
var PromRegistry = prometheus.NewRegistry()

var (
	RetentionDeleted = promauto.With(PromRegistry).NewCounterVec(prometheus.CounterOpts{
		Name: "pooml_retention_deleted_total",
		Help: "Rows deleted by the hourly retention sweep.",
	}, []string{"table"})

	BackupRuns = promauto.With(PromRegistry).NewCounterVec(prometheus.CounterOpts{
		Name: "pooml_backup_runs_total",
		Help: "Backup runs by outcome.",
	}, []string{"outcome"})
)

func init() {
	PromRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// RegisterDBSizeMetrics samples on-disk DB sizes at scrape time.
func RegisterDBSizeMetrics(dbDir string) {
	for _, name := range []string{"logs", "metrics", "meta"} {
		path := filepath.Join(dbDir, name+".db")
		PromRegistry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "pooml_db_size_bytes",
			Help:        "SQLite file size on disk.",
			ConstLabels: prometheus.Labels{"db": name},
		}, func() float64 {
			st, err := os.Stat(path)
			if err != nil {
				return 0
			}
			return float64(st.Size())
		}))
	}
}
