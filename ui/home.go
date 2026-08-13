package ui

import (
	"context"
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

	rows, err := ur.Pools.LogsRead.QueryContext(ctx,
		"SELECT service, MAX(timestamp), COUNT(*) FROM logs GROUP BY service ORDER BY 2 DESC LIMIT 25")
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

func (ur *Router) homeFragmentError(w http.ResponseWriter, req *http.Request, err error) {
	log.Error().Err(err).Str("path", req.URL.Path).Msg("home fragment query")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK) // htmx swaps 200s; the fragment retries in 30s anyway
	_, _ = w.Write([]byte(`<p class="text-sm opacity-60 italic">That didn't load. Trying again shortly.</p>`))
}
