package ui

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/rs/zerolog/log"
)

// Home fragments run fixed internal queries on the read pool - not user
// input, so no validation pipeline, just a tight timeout.
const homeQueryTimeout = 3 * time.Second

const volumeBucketMs = 5 * 60 * 1000

func (ur *Router) homePageVolumeSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	nowMs := time.Now().UnixMilli()
	fromMs := nowMs - 60*60*1000

	rows, err := ur.Pools.LogsRead.QueryContext(ctx,
		"SELECT (timestamp / ?) * ? AS bucket, COUNT(*) FROM logs WHERE timestamp > ? GROUP BY bucket",
		volumeBucketMs, volumeBucketMs, fromMs)
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	defer rows.Close()

	counts := make(map[int64]int64)
	for rows.Next() {
		var bucket, count int64
		if err := rows.Scan(&bucket, &count); err != nil {
			ur.homeFragmentError(w, req, err)
			return
		}
		counts[bucket] = count
	}
	if err := rows.Err(); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}

	var view templates.HomeVolumeView
	for t := (fromMs / volumeBucketMs) * volumeBucketMs; t <= nowMs; t += volumeBucketMs {
		c := counts[t]
		view.Points = append(view.Points, templates.VolumePoint{BucketMs: t, Count: c})
		view.Total += c
		if c > view.Max {
			view.Max = c
		}
	}
	ur.render(w, req, http.StatusOK, templates.HomeVolume(view))
}

func (ur *Router) homePageErrorsSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	rows, err := ur.Pools.LogsRead.QueryContext(ctx,
		"SELECT service, COUNT(*) AS cnt FROM logs WHERE level >= ? AND timestamp > ? GROUP BY service ORDER BY cnt DESC LIMIT 10",
		common.LevelError, time.Now().UnixMilli()-24*60*60*1000)
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	defer rows.Close()

	var out []templates.HomeErrorRow
	for rows.Next() {
		var r templates.HomeErrorRow
		if err := rows.Scan(&r.Service, &r.Count); err != nil {
			ur.homeFragmentError(w, req, err)
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	ur.render(w, req, http.StatusOK, templates.HomeErrors(out))
}

func (ur *Router) homePageServicesSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	// bounded to 7 days: an unbounded GROUP BY walks the whole service index
	// on every 30s refresh, which hurts at multi-million-row scale. A service
	// silent for a week has no place on a "current state" home card anyway.
	rows, err := ur.Pools.LogsRead.QueryContext(ctx,
		"SELECT service, MAX(timestamp), COUNT(*) FROM logs WHERE timestamp > ? GROUP BY service ORDER BY 2 DESC LIMIT 25",
		time.Now().UnixMilli()-7*24*60*60*1000)
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	defer rows.Close()

	var out []templates.HomeServiceRow
	for rows.Next() {
		var r templates.HomeServiceRow
		if err := rows.Scan(&r.Service, &r.LastSeenMs, &r.Count); err != nil {
			ur.homeFragmentError(w, req, err)
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	ur.render(w, req, http.StatusOK, templates.HomeServices(out))
}

// homePageMetricsServicesSegment mirrors the logs services card for the
// metrics signal (M13 parity): same 7-day bound, same reasoning.
func (ur *Router) homePageMetricsServicesSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	rows, err := ur.Pools.LogsRead.QueryContext(ctx,
		"SELECT service, MAX(timestamp), COUNT(*) FROM metrics WHERE timestamp > ? GROUP BY service ORDER BY 2 DESC LIMIT 25",
		time.Now().UnixMilli()-7*24*60*60*1000)
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	defer rows.Close()

	var out []templates.HomeServiceRow
	for rows.Next() {
		var r templates.HomeServiceRow
		if err := rows.Scan(&r.Service, &r.LastSeenMs, &r.Count); err != nil {
			ur.homeFragmentError(w, req, err)
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	ur.render(w, req, http.StatusOK, templates.HomeMetricsServices(out))
}

// homePageMetricsSegment is ingest HEALTH, not metric values: values need
// user-defined thresholds to mean "on fire" (that's what alerts are for);
// failing scrapers and silent pipelines are facts.
func (ur *Router) homePageMetricsSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	var view templates.HomeMetricsView

	rows, err := ur.Pools.Meta.QueryContext(ctx,
		"SELECT service, last_error FROM scrape_targets WHERE enabled = 1 AND last_error IS NOT NULL ORDER BY last_error_at DESC LIMIT 5")
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t templates.FailingTarget
		if err := rows.Scan(&t.Service, &t.Error); err != nil {
			ur.homeFragmentError(w, req, err)
			return
		}
		view.FailingTargets = append(view.FailingTargets, t)
	}
	if err := rows.Err(); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}

	var lastTs sql.NullInt64
	if err := ur.Pools.LogsRead.QueryRowContext(ctx, "SELECT MAX(timestamp) FROM metrics").Scan(&lastTs); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	view.LastSeenMs = lastTs.Int64
	if err := ur.Pools.LogsRead.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM metrics WHERE timestamp > ?", time.Now().UnixMilli()-24*60*60*1000).Scan(&view.Points24h); err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	ur.render(w, req, http.StatusOK, templates.HomeMetrics(view))
}

// A writer has to fail for this long before Home says so: one failed flush
// that succeeds on the next try is not news.
const writerFailingAfterMs = 60 * 1000

// homePageAttentionSegment surfaces what has quietly stopped working: writers
// that cannot store, alerts that cannot run or cannot deliver, failing backups.
// Empty body when all is well: the card only exists while something is wrong.
func (ur *Router) homePageAttentionSegment(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), homeQueryTimeout)
	defer cancel()

	var view templates.HomeAttentionView
	now := time.Now().UnixMilli()
	for _, wr := range []struct {
		name   string
		health *common.WriterHealth
	}{{"logs", ur.LogWriter}, {"metrics", ur.MetricWriter}} {
		if since, msg := wr.health.Failing(); since > 0 && now-since >= writerFailingAfterMs {
			view.Writers = append(view.Writers, templates.FailingWriter{Name: wr.name, Error: msg, SinceMs: since})
		}
	}

	alerts, err := ur.Alerts.ListEnabled(ctx)
	if err != nil {
		ur.homeFragmentError(w, req, err)
		return
	}
	for _, a := range alerts {
		switch {
		case a.QueryBroken(): // same rule the evaluator announces on
			view.Blind = append(view.Blind, templates.BrokenAlert{Name: a.Name, Error: a.LastError.String})
		case a.FailureCount == 0 && a.LastError.Valid:
			// the query ran; the notification did not get out
			view.Undelivered = append(view.Undelivered, templates.BrokenAlert{Name: a.Name, Error: a.LastError.String})
		}
	}

	// only while backups are switched on: a failed manual run on a disabled
	// schedule is not something to keep reminding about
	if b := ur.backupView(ctx); b.Enabled && b.LastResult != "" && b.LastResult != "ok" {
		view.BackupError, view.BackupAtMs = b.LastResult, b.LastRunAt
	}
	ur.render(w, req, http.StatusOK, templates.HomeAttention(view))
}

func (ur *Router) homeFragmentError(w http.ResponseWriter, req *http.Request, err error) {
	log.Error().Err(err).Str("path", req.URL.Path).Msg("home fragment query")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK) // htmx swaps 200s; the fragment retries in 30s anyway
	_, _ = w.Write([]byte(`<p class="text-sm opacity-60 italic">That didn't load. Trying again shortly.</p>`))
}
