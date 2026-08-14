package ui

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

// defaultMetricsQuery is a catalog view: which metrics exist, from which
// services, how fresh - the natural first question on this page.
const defaultMetricsQuery = "SELECT name, service, COUNT(*) AS points, MAX(timestamp) AS last_seen FROM metrics GROUP BY name, service ORDER BY last_seen DESC LIMIT 100"

var explorerViews = map[string]bool{"auto": true, "line": true, "bar": true, "stat": true, "table": true}

func (ur *Router) metricsExplorerPage(w http.ResponseWriter, req *http.Request) {
	ur.renderExplorer(w, req, strings.TrimSpace(req.URL.Query().Get("q")), "")
}

func (ur *Router) renderExplorer(w http.ResponseWriter, req *http.Request, q, saveErr string) {
	if q == "" {
		q = defaultMetricsQuery
	}
	viewSel := req.URL.Query().Get("view")
	if !explorerViews[viewSel] {
		viewSel = "auto"
	}
	view := templates.MetricsExplorerView{Query: q, View: viewSel, SaveErr: saveErr}

	v, err := query.ValidateIn(q, query.ScopeMetrics)
	if err != nil {
		view.Shaped = templates.ShapedResult{Kind: "error", ErrMsg: humanizeSQLError(err)}
	} else if res, execErr := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL()); execErr != nil {
		view.Shaped = templates.ShapedResult{Kind: "error", ErrMsg: execErr.Error()}
	} else {
		chartType := viewSel
		if chartType == "auto" {
			chartType = ""
		}
		view.Shaped = shapeResult(res, chartType, "explorer")
	}

	switch view.Shaped.Kind {
	case "empty":
		// two very different empty states: nothing ingested at all vs a
		// query that matched nothing
		var n int
		if err := ur.Pools.LogsRead.QueryRowContext(req.Context(), "SELECT EXISTS(SELECT 1 FROM metrics)").Scan(&n); err == nil {
			view.HasAnyMetrics = n == 1
		}
	case "chart", "stat", "table":
		if dashboards, err := ur.Dashboards.ListDashboards(req.Context()); err == nil {
			view.Dashboards = dashboards
		}
	}

	status := http.StatusOK
	if view.Shaped.Kind == "error" || saveErr != "" {
		status = http.StatusBadRequest
	}
	ur.render(w, req, status, templates.MetricsExplorerPage(view, nosurf.Token(req)))
}

// saveExplorerPanel turns the current explorer query into a dashboard panel.
func (ur *Router) saveExplorerPanel(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Redirect(w, req, "/metrics-explorer", http.StatusSeeOther)
		return
	}
	dashboardID, _ := strconv.ParseInt(req.PostFormValue("dashboard_id"), 10, 64)
	chartType := req.PostFormValue("chart_type")
	if chartType == "auto" || chartType == "table" {
		chartType = "" // auto-detect on the dashboard; "table" isn't a panel type
	}
	p := services.Panel{
		DashboardID: dashboardID,
		Title:       req.PostFormValue("title"),
		Query:       req.PostFormValue("query"),
		ChartType:   chartType,
		Width:       2,
	}
	if err := ur.Dashboards.CreatePanel(req.Context(), p); err != nil {
		ur.renderExplorer(w, req, p.Query, err.Error())
		return
	}
	http.Redirect(w, req, fmt.Sprintf("/dashboards/%d", dashboardID), http.StatusSeeOther)
}

func (ur *Router) exportMetrics(w http.ResponseWriter, req *http.Request) {
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if q == "" {
		q = defaultMetricsQuery
	}
	v, err := query.ValidateIn(q, query.ScopeMetrics)
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
