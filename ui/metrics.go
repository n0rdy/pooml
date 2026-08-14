package ui

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

// defaultMetricsQuery is a catalog view: which metrics exist, from which
// services, how fresh - the natural first question on this page.
const defaultMetricsQuery = "SELECT name, service, COUNT(*) AS points, MAX(timestamp) AS last_seen FROM metrics GROUP BY name, service ORDER BY last_seen DESC LIMIT 100"

func (ur *Router) metricsExplorerPage(w http.ResponseWriter, req *http.Request) {
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if q == "" {
		q = defaultMetricsQuery
	}
	view := templates.MetricsExplorerView{Query: q}

	v, err := query.Validate(q)
	if err != nil {
		view.ErrMsg = humanizeSQLError(err)
	} else if res, execErr := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL()); execErr != nil {
		view.ErrMsg = execErr.Error()
	} else {
		view.Columns = res.Columns
		view.Rows = make([][]string, len(res.Rows))
		for i, row := range res.Rows {
			cells := make([]string, len(row))
			for j, cell := range row {
				cells[j] = cellString(cell)
			}
			view.Rows[i] = cells
		}
	}

	status := http.StatusOK
	if view.ErrMsg != "" {
		status = http.StatusBadRequest
	}
	ur.render(w, req, status, templates.MetricsExplorerPage(view, nosurf.Token(req)))
}

func (ur *Router) exportMetrics(w http.ResponseWriter, req *http.Request) {
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if q == "" {
		q = defaultMetricsQuery
	}
	v, err := query.Validate(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="metrics.json"`)
		out := make([]map[string]any, len(res.Rows))
		for i, row := range res.Rows {
			m := make(map[string]any, len(res.Columns))
			for j, col := range res.Columns {
				m[col] = row[j]
			}
			out[i] = m
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Error().Err(err).Msg("metrics json export")
		}
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="metrics.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(res.Columns)
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = cellString(cell)
		}
		_ = cw.Write(cells)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Error().Err(err).Msg("metrics csv export")
	}
}
