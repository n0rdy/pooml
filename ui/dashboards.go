package ui

import (
	"fmt"
	"net/http"
	"strconv"

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
	id, err := ur.Dashboards.CreateDashboard(req.Context(), req.PostFormValue("name"), req.PostFormValue("description"))
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
	// full navigation for HTMX callers too; the page they were on is gone
	w.Header().Set("HX-Redirect", "/dashboards")
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
	ur.render(w, req, status, templates.DashboardPage(d, panels, errMsg, nosurf.Token(req)))
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
	}
	return p, nil
}

func (ur *Router) createPanel(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p, err := panelFromForm(req, id)
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
		err = ur.Dashboards.UpdatePanel(req.Context(), p)
	}
	if err != nil {
		ur.render(w, req, http.StatusBadRequest, templates.PanelEditCard(p, err.Error(), nosurf.Token(req)))
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
	if req.URL.Query().Get("edit") == "1" {
		ur.render(w, req, http.StatusOK, templates.PanelEditCard(p, "", nosurf.Token(req)))
		return
	}
	// edit-cancel restores the whole card
	if req.URL.Query().Get("card") == "1" {
		ur.render(w, req, http.StatusOK, templates.PanelCard(p, nosurf.Token(req)))
		return
	}

	view := templates.PanelView{Panel: p}
	v, err := query.Validate(p.Query)
	if err != nil {
		view.Kind, view.ErrMsg = "error", humanizeSQLError(err)
	} else if res, execErr := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL()); execErr != nil {
		view.Kind, view.ErrMsg = "error", execErr.Error()
	} else {
		shapePanel(&view, res)
	}
	ur.render(w, req, http.StatusOK, templates.PanelFragment(view))
}

// shapePanel turns a query result into a renderable panel: explicit
// chart_type wins, otherwise the shape decides. See CONTEXT.md > Metrics >
// Dashboards for the conventions.
func shapePanel(view *templates.PanelView, res *query.Result) {
	if len(res.Rows) == 0 {
		view.Kind = "empty"
		return
	}

	numericCols := numericColumns(res)

	if view.Panel.ChartType == "stat" || (view.Panel.ChartType == "" && len(res.Rows) == 1 && len(res.Columns) == 1 && numericCols[0]) {
		for j := range res.Columns {
			if numericCols[j] {
				view.Kind, view.Stat = "stat", formatStat(res.Rows[0][j])
				return
			}
		}
		view.Kind, view.ErrMsg = "error", "stat needs a numeric column"
		return
	}

	// x axis = first column; series = every other numeric column
	var datasets []templates.ChartDataset
	for j := 1; j < len(res.Columns); j++ {
		if !numericCols[j] {
			continue
		}
		data := make([]float64, len(res.Rows))
		for i, row := range res.Rows {
			data[i], _ = asFloat(row[j])
		}
		datasets = append(datasets, templates.ChartDataset{Label: res.Columns[j], Data: data})
	}
	if len(datasets) == 0 {
		// nothing chartable: show the rows instead of erroring
		view.Kind = "table"
		view.Columns = res.Columns
		view.Rows = make([][]string, len(res.Rows))
		for i, row := range res.Rows {
			cells := make([]string, len(row))
			for j, cell := range row {
				cells[j] = cellString(cell)
			}
			view.Rows[i] = cells
		}
		return
	}

	timeX := colLooksLikeEpochMs(res, 0)
	chartType := view.Panel.ChartType
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
	} else {
		payload.Labels = make([]string, len(res.Rows))
		for i, row := range res.Rows {
			payload.Labels[i] = cellString(row[0])
		}
	}
	view.Kind, view.Chart = "chart", payload
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
