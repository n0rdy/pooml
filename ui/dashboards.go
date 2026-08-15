package ui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/go-chi/chi/v5"
	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

func (ur *Router) dashboardsPage(w http.ResponseWriter, req *http.Request) {
	ur.renderDashboardsList(w, req, "", http.StatusOK)
}

func (ur *Router) renderDashboardsList(w http.ResponseWriter, req *http.Request, errMsg string, status int) {
	dashboards, err := ur.Dashboards.ListDashboards(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("list dashboards")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	ur.render(w, req, status, templates.DashboardsListPage(dashboards, errMsg, nosurf.Token(req)))
}

func (ur *Router) createDashboard(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Redirect(w, req, "/dashboards", http.StatusSeeOther)
		return
	}
	id, err := ur.Dashboards.CreateDashboard(req.Context(), req.PostFormValue("name"), req.PostFormValue("type"), req.PostFormValue("description"))
	if err != nil {
		ur.renderDashboardsList(w, req, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, req, fmt.Sprintf("/dashboards/%d", id), http.StatusSeeOther)
}

func (ur *Router) deleteDashboard(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := ur.Dashboards.DeleteDashboard(req.Context(), id); err != nil {
		log.Error().Err(err).Int64("id", id).Msg("delete dashboard")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	// full navigation for HTMX callers too; the page they were on is gone.
	// HX-Redirect must ride a 200: on a real 3xx, XHR follows the Location
	// transparently and htmx never sees the header (nothing happens).
	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/dashboards")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, req, "/dashboards", http.StatusSeeOther)
}

func (ur *Router) dashboardPage(w http.ResponseWriter, req *http.Request) {
	ur.renderDashboard(w, req, "", http.StatusOK)
}

func (ur *Router) renderDashboard(w http.ResponseWriter, req *http.Request, errMsg string, status int) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	d, err := ur.Dashboards.GetDashboard(req.Context(), id)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	panels, err := ur.Dashboards.ListPanels(req.Context(), id)
	if err != nil {
		log.Error().Err(err).Int64("id", id).Msg("list panels")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	var metricNames []string
	if d.Type == "metrics" {
		metricNames, _ = ur.metricNames(req) // feeds the quick-query autocomplete
	}
	ur.render(w, req, status, templates.DashboardPage(d, panels, metricNames, errMsg, nosurf.Token(req)))
}

func panelFromForm(req *http.Request, dashboardID int64) (services.Panel, error) {
	var p services.Panel
	if err := req.ParseForm(); err != nil {
		return p, err
	}
	width, _ := strconv.ParseInt(req.PostFormValue("width"), 10, 64)
	p = services.Panel{
		DashboardID: dashboardID,
		Title:       req.PostFormValue("title"),
		Query:       req.PostFormValue("query"),
		ChartType:   req.PostFormValue("chart_type"),
		Width:       width,
		DSL:         strings.TrimSpace(req.PostFormValue("dsl")),
		FTS:         strings.TrimSpace(req.PostFormValue("fts")),
		Op:          req.PostFormValue("op"),
	}
	return p, nil
}

// applyPanelDSL enforces the same authority rule as the explorer: when the
// panel carries a DSL expression, the server recompiles it and THAT becomes
// the stored query - a save inside the client's compile debounce can never
// persist stale SQL.
func (ur *Router) applyPanelDSL(req *http.Request, p *services.Panel) error {
	if p.DSL == "" {
		return nil
	}
	d, err := query.ParseDSL(p.DSL)
	if err != nil {
		return fmt.Errorf("quick query: %w", err)
	}
	var services []string
	if d.NeedsServices() {
		services = ur.servicesForMetric(req, d.Metric)
	}
	sql, err := d.SQL(services)
	if err != nil {
		return fmt.Errorf("quick query: %w", err)
	}
	p.Query = sql
	return nil
}

func (ur *Router) createPanel(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p, err := panelFromForm(req, id)
	if err == nil {
		err = ur.applyPanelDSL(req, &p)
	}
	if err == nil {
		err = ur.Dashboards.CreatePanel(req.Context(), p)
	}
	if err != nil {
		ur.renderDashboard(w, req, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, req, fmt.Sprintf("/dashboards/%d", id), http.StatusSeeOther)
}

func (ur *Router) updatePanel(w http.ResponseWriter, req *http.Request) {
	dashboardID, err1 := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	panelID, err2 := strconv.ParseInt(chi.URLParam(req, "pid"), 10, 64)
	if err1 != nil || err2 != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p, err := panelFromForm(req, dashboardID)
	p.ID = panelID
	if err == nil {
		err = ur.applyPanelDSL(req, &p)
	}
	if err == nil {
		err = ur.Dashboards.UpdatePanel(req.Context(), p)
	}
	if err != nil {
		dtype := "metrics"
		if d, gerr := ur.Dashboards.GetDashboard(req.Context(), dashboardID); gerr == nil {
			dtype = d.Type
		}
		ur.render(w, req, http.StatusBadRequest, templates.PanelEditCard(p, dtype, err.Error(), nosurf.Token(req)))
		return
	}
	// fresh card; its body reloads via hx-trigger="load"
	ur.render(w, req, http.StatusOK, templates.PanelCard(p, nosurf.Token(req)))
}

func (ur *Router) deletePanel(w http.ResponseWriter, req *http.Request) {
	dashboardID, err1 := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	panelID, err2 := strconv.ParseInt(chi.URLParam(req, "pid"), 10, 64)
	if err1 != nil || err2 != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := ur.Dashboards.DeletePanel(req.Context(), dashboardID, panelID); err != nil {
		log.Error().Err(err).Int64("panel", panelID).Msg("delete panel")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	// empty 200: the card swaps itself away
	w.WriteHeader(http.StatusOK)
}

// panelFragment is the auto-refreshing panel body: runs the panel's query and
// returns the shaped result (stat, chart canvas + data, or table fallback).
// ?edit=1 returns the inline edit form instead.
func (ur *Router) panelFragment(w http.ResponseWriter, req *http.Request) {
	dashboardID, err1 := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	panelID, err2 := strconv.ParseInt(chi.URLParam(req, "pid"), 10, 64)
	if err1 != nil || err2 != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p, err := ur.Dashboards.GetPanel(req.Context(), dashboardID, panelID)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	d, err := ur.Dashboards.GetDashboard(req.Context(), dashboardID)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if req.URL.Query().Get("edit") == "1" {
		ur.render(w, req, http.StatusOK, templates.PanelEditCard(p, d.Type, "", nosurf.Token(req)))
		return
	}
	// edit-cancel restores the whole card
	if req.URL.Query().Get("card") == "1" {
		ur.render(w, req, http.StatusOK, templates.PanelCard(p, nosurf.Token(req)))
		return
	}

	view := templates.PanelView{Panel: p}
	v, err := query.ValidateIn(p.Query, services.PanelScope(d.Type))
	// logs panels combine their stored search text at render, exactly like
	// the logs page does with the FTS bar
	var args []any
	if err == nil && d.Type == "logs" && p.FTS != "" {
		op := p.Op
		if op != "or" {
			op = "and"
		}
		if cerr := v.CombineFTS(op); cerr == nil {
			args = append(args, query.FTSMatch(p.FTS))
		}
	}
	if err != nil {
		view.ShapedResult = templates.ShapedResult{Kind: "error", ErrMsg: humanizeSQLError(err)}
	} else if res, execErr := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL(), args...); execErr != nil {
		view.ShapedResult = templates.ShapedResult{Kind: "error", ErrMsg: execErr.Error()}
	} else if d.Type == "logs" && (p.ChartType == "stream" || p.ChartType == "") {
		// the primary logs panel: latest matching lines, viewer-styled
		view.Kind = "stream"
		colIdx := make(map[string]int, len(res.Columns))
		for i, c := range res.Columns {
			colIdx[c] = i
		}
		for _, row := range res.Rows {
			if r, ok := rowFromResult(colIdx, row); ok {
				view.StreamRows = append(view.StreamRows, r)
			}
		}
		// same convention as the log viewer: oldest at the top, newest at
		// the bottom (the pane bottom-anchors client-side)
		sort.Slice(view.StreamRows, func(i, j int) bool {
			a, b := view.StreamRows[i], view.StreamRows[j]
			if a.Ts != b.Ts {
				return a.Ts < b.Ts
			}
			return a.ID < b.ID
		})
	} else {
		view.ShapedResult = shapeResult(res, logsKindToChartType(d.Type, p.ChartType), fmt.Sprintf("p%d", p.ID), nil)
	}
	ur.render(w, req, http.StatusOK, templates.PanelFragment(view))
}

// logsKindToChartType maps logs panel kinds onto the shared shaping:
// chart auto-detects line/bar, number is the stat.
func logsKindToChartType(dashboardType, kind string) string {
	if dashboardType != "logs" {
		return kind
	}
	if kind == "number" {
		return "stat"
	}
	return "" // "chart": auto-detect
}

// chartFill is the exact bucket grid a DSL query asked for, letting the
// chart show the WHOLE requested window: zero-filled for counter verbs (no
// rows = zero events, a fact), null gaps for gauge verbs (no samples =
// unknown - filling 0 would fabricate a measurement). Only the DSL path can
// provide this; inferring a grid from raw SQL results would be guessing.
type chartFill struct {
	BucketMs int64
	StartMs  int64
	EndMs    int64
	Zero     bool
}

const maxFillBuckets = 5000

func (f *chartFill) buckets() []int64 {
	n := (f.EndMs - f.StartMs) / f.BucketMs
	if n <= 0 || n > maxFillBuckets {
		return nil
	}
	out := make([]int64, 0, n+1)
	for b := f.StartMs; b <= f.EndMs; b += f.BucketMs {
		out = append(out, b)
	}
	return out
}

// shapeResult turns a query result into a renderable shape, shared by panels
// and the metrics explorer: explicit chartType wins ("table" forces raw
// rows), otherwise the result shape decides. Columns/Rows are always filled
// so charts can offer the raw rows alongside. See CONTEXT.md > Metrics >
// Dashboards for the conventions.
func shapeResult(res *query.Result, chartType, chartID string, fill *chartFill) templates.ShapedResult {
	s := templates.ShapedResult{ChartID: chartID, Columns: res.Columns, Rows: cellsOf(res)}
	if len(res.Rows) == 0 {
		// zero rows over a known counter grid IS data: a flat zero line
		if fill != nil && fill.Zero && chartType != "table" && chartType != "stat" && len(res.Columns) >= 2 {
			if buckets := fill.buckets(); buckets != nil {
				datasets := make([]templates.ChartDataset, 0, len(res.Columns)-1)
				for _, col := range res.Columns[1:] {
					data := make([]*float64, len(buckets))
					for i := range data {
						z := 0.0
						data[i] = &z
					}
					datasets = append(datasets, templates.ChartDataset{Label: col, Data: data})
				}
				if chartType == "" {
					chartType = "line"
				}
				s.Kind = "chart"
				s.Chart = &templates.ChartPayload{Type: chartType, LabelsMs: buckets, Datasets: datasets}
				return s
			}
		}
		s.Kind = "empty"
		return s
	}
	if chartType == "table" {
		s.Kind = "table"
		return s
	}

	numericCols := numericColumns(res)

	if chartType == "stat" || (chartType == "" && len(res.Rows) == 1 && len(res.Columns) == 1 && numericCols[0]) {
		for j := range res.Columns {
			if numericCols[j] {
				s.Kind, s.Stat = "stat", formatStat(res.Rows[0][j])
				return s
			}
		}
		s.Kind, s.ErrMsg = "error", "stat needs a numeric column"
		return s
	}

	// x axis = first column; series = every other numeric column. Epoch-ms
	// columns never chart as series: a timestamp is an axis, and its
	// magnitude (~1.7e12) flattens every real value next to it.
	var datasets []templates.ChartDataset
	for j := 1; j < len(res.Columns); j++ {
		if !numericCols[j] || colLooksLikeEpochMs(res, j) {
			continue
		}
		data := make([]*float64, len(res.Rows))
		for i, row := range res.Rows {
			if f, ok := asFloat(row[j]); ok {
				v := f
				data[i] = &v
			}
		}
		datasets = append(datasets, templates.ChartDataset{Label: res.Columns[j], Data: data})
	}
	if len(datasets) == 0 {
		// nothing chartable: show the rows instead of erroring
		s.Kind = "table"
		return s
	}

	timeX := colLooksLikeEpochMs(res, 0)
	if chartType == "" {
		if timeX {
			chartType = "line"
		} else {
			chartType = "bar"
		}
	}
	payload := &templates.ChartPayload{Type: chartType, Datasets: datasets}
	if timeX {
		payload.LabelsMs = make([]int64, len(res.Rows))
		for i, row := range res.Rows {
			payload.LabelsMs[i] = asInt64(row[0])
		}
		// re-index sparse results onto the full requested grid
		if fill != nil {
			if buckets := fill.buckets(); buckets != nil {
				idx := make(map[int64]int, len(res.Rows))
				for i, row := range res.Rows {
					idx[asInt64(row[0])] = i
				}
				for di := range datasets {
					filled := make([]*float64, len(buckets))
					for bi, b := range buckets {
						if ri, ok := idx[b]; ok {
							filled[bi] = datasets[di].Data[ri]
						} else if fill.Zero {
							z := 0.0
							filled[bi] = &z
						}
					}
					datasets[di].Data = filled
				}
				payload.LabelsMs = buckets
			}
		}
	} else {
		payload.Labels = make([]string, len(res.Rows))
		for i, row := range res.Rows {
			payload.Labels[i] = cellString(row[0])
		}
	}
	s.Kind, s.Chart = "chart", payload
	return s
}

func cellsOf(res *query.Result) [][]string {
	rows := make([][]string, len(res.Rows))
	for i, row := range res.Rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = cellString(cell)
		}
		rows[i] = cells
	}
	return rows
}

func numericColumns(res *query.Result) map[int]bool {
	numeric := map[int]bool{}
	for j := range res.Columns {
		hasValue := false
		ok := true
		for _, row := range res.Rows {
			if row[j] == nil {
				continue
			}
			if _, isNum := asFloat(row[j]); !isNum {
				ok = false
				break
			}
			hasValue = true
		}
		numeric[j] = ok && hasValue
	}
	return numeric
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}

// colLooksLikeEpochMs: every non-NULL value is numeric and at least year-2001
// in milliseconds. Good enough to pick a time axis over categories.
func colLooksLikeEpochMs(res *query.Result, col int) bool {
	seen := false
	for _, row := range res.Rows {
		if row[col] == nil {
			continue
		}
		f, ok := asFloat(row[col])
		if !ok || f < 1e12 {
			return false
		}
		seen = true
	}
	return seen
}

func formatStat(v any) string {
	if f, ok := asFloat(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return cellString(v)
}
