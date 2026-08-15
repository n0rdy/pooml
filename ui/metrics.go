package ui

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

// defaultMetricsQuery is a catalog view: which metrics exist, from which
// services, how fresh - the natural first question on this page.
const defaultMetricsQuery = `SELECT name, service, COUNT(*) AS points, MAX(timestamp) AS last_seen
FROM metrics
GROUP BY name, service
ORDER BY last_seen DESC
LIMIT 100`

var explorerViews = map[string]bool{"auto": true, "line": true, "bar": true, "stat": true, "table": true}

func (ur *Router) metricsExplorerPage(w http.ResponseWriter, req *http.Request) {
	ur.renderExplorer(w, req, strings.TrimSpace(req.URL.Query().Get("q")), "")
}

type dslFilterJSON struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// dslParsedJSON is the structured form of a DSL expression: the query
// builder's only source of grammar knowledge (no grammar in JS, ever).
type dslParsedJSON struct {
	Verb      string          `json:"verb"`
	Metric    string          `json:"metric"`
	Filters   []dslFilterJSON `json:"filters"`
	PerMs     int64           `json:"perMs"`
	LastMs    int64           `json:"lastMs"`
	ByService bool            `json:"byService"`
}

type dslCompileResponse struct {
	SQL    string         `json:"sql,omitempty"`
	Error  string         `json:"error,omitempty"`
	Hint   string         `json:"hint,omitempty"`
	Parsed *dslParsedJSON `json:"parsed,omitempty"`
}

// rawValueVerbs chart a counter's cumulative value as-is - always a straight
// climb, never the change the user meant. The stored metric TYPE is data, so
// hinting on it is not the name-guessing we rejected.
var rawValueVerbs = map[string]bool{"avg": true, "min": true, "max": true, "sum": true}

func (ur *Router) counterHint(req *http.Request, d *query.DSLQuery) string {
	if !rawValueVerbs[d.Verb] {
		return ""
	}
	var typ int
	err := ur.Pools.LogsRead.QueryRowContext(req.Context(),
		"SELECT type FROM metrics WHERE name = ? LIMIT 1", d.Metric).Scan(&typ)
	if err != nil || typ != 0 {
		return ""
	}
	return fmt.Sprintf("%s is a counter - it only climbs, so %s() of its raw value draws a straight line up. increase() or rate() shows the change.", d.Metric, d.Verb)
}

// compileDSL serves the live one-way mirror: DSL in, SQL (plus the parsed
// structure, for the builder) out. Errors ride the 200 - mid-typing input is
// expected to be broken and the client renders them as a hint, not a failure.
func (ur *Router) compileDSL(w http.ResponseWriter, req *http.Request) {
	respond := func(payload dslCompileResponse) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			log.Error().Err(err).Msg("encode dsl compile response")
		}
	}
	d, err := query.ParseDSL(req.URL.Query().Get("dsl"))
	if err != nil {
		respond(dslCompileResponse{Error: err.Error()})
		return
	}
	var services []string
	if d.NeedsServices() {
		services = ur.servicesForMetric(req, d.Metric)
	}
	sql, err := d.SQL(services)
	if err != nil {
		respond(dslCompileResponse{Error: err.Error()})
		return
	}
	// the compiler must never emit SQL its own explorer would reject
	if _, err := query.ValidateIn(sql, query.ScopeMetrics); err != nil {
		log.Error().Err(err).Str("dsl", req.URL.Query().Get("dsl")).Msg("DSL compiled invalid SQL")
		respond(dslCompileResponse{Error: "internal compile error; please report this one"})
		return
	}
	parsed := &dslParsedJSON{
		Verb:      d.Verb,
		Metric:    d.Metric,
		Filters:   make([]dslFilterJSON, 0, len(d.Filters)),
		PerMs:     d.BucketMs,
		LastMs:    d.WindowMs,
		ByService: d.ByService,
	}
	for _, f := range d.Filters {
		parsed.Filters = append(parsed.Filters, dslFilterJSON{Key: f.Key, Value: f.Value})
	}
	respond(dslCompileResponse{SQL: sql, Parsed: parsed, Hint: ur.counterHint(req, d)})
}

func (ur *Router) renderExplorer(w http.ResponseWriter, req *http.Request, q, saveErr string) {
	dsl := strings.TrimSpace(req.URL.Query().Get("dsl"))
	// a dsl-only URL is a real query, not the landing catalog
	landing := q == "" && dsl == ""
	if q == "" {
		q = defaultMetricsQuery
	}
	viewSel := req.URL.Query().Get("view")
	if !explorerViews[viewSel] {
		viewSel = "auto"
	}
	if landing {
		viewSel = "table" // the landing catalog is a listing, not a chart
	}
	view := templates.MetricsExplorerView{
		Query:   q,
		View:    viewSel,
		SaveErr: saveErr,
		DSL:     dsl,
	}
	if names, err := ur.metricNames(req); err == nil {
		view.MetricNames = names
	}

	// the landing catalog is clickable: each metric links to a DSL query
	// with the verb picked by its STORED TYPE (counter - increase, gauge -
	// avg) - data-driven, so you never chart a counter's raw climb by
	// accident. Empty catalog falls through to the onboarding empty state.
	if landing {
		if catalog := ur.metricsCatalog(req); len(catalog) > 0 {
			view.Catalog = catalog
			view.Snippets = explorerSnippets
			ur.render(w, req, http.StatusOK, templates.MetricsExplorerPage(view, nosurf.Token(req)))
			return
		}
	}

	// dsl is authoritative when present: recompile it server-side so a Run
	// landing inside the client's compile debounce window can never submit
	// stale SQL (the "unchecked by service but the pivot survived" race).
	// It also yields the exact bucket grid for whole-window gap filling.
	var fill *chartFill
	if view.DSL != "" {
		if d, perr := query.ParseDSL(view.DSL); perr == nil {
			var services []string
			if d.NeedsServices() {
				services = ur.servicesForMetric(req, d.Metric)
			}
			if sql, serr := d.SQL(services); serr == nil {
				q = sql
				view.Query = sql
				if d.BucketMs > 0 && d.Verb != "latest" {
					now := time.Now().UnixMilli()
					fill = &chartFill{
						BucketMs: d.BucketMs,
						StartMs:  (now - d.WindowMs) / d.BucketMs * d.BucketMs,
						EndMs:    now / d.BucketMs * d.BucketMs,
						Zero:     d.Verb == "increase" || d.Verb == "rate" || d.Verb == "count",
					}
				}
			}
		}
	}

	if strings.Contains(q, snippetPlaceholderMarker) {
		view.Shaped = templates.ShapedResult{Kind: "error", ErrMsg: placeholderErr(q)}
		view.Snippets = explorerSnippets
		ur.render(w, req, http.StatusBadRequest, templates.MetricsExplorerPage(view, nosurf.Token(req)))
		return
	}

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
		view.Shaped = shapeResult(res, chartType, "explorer", fill)
	}

	view.Snippets = explorerSnippets

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

var labelKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// filterOptions feeds the builder's filter datalists: without `key`, the
// filterable keys for a metric (service/host plus its actual label keys);
// with `key`, the values that exist for it. Offering what exists for the
// user to pick is a menu, not name-guessing.
func (ur *Router) filterOptions(w http.ResponseWriter, req *http.Request) {
	metric := req.URL.Query().Get("metric")
	key := req.URL.Query().Get("key")
	respond := func(options []string) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string][]string{"options": options}); err != nil {
			log.Error().Err(err).Msg("encode filter options")
		}
	}

	if key == "" {
		keys := []string{"service", "host"}
		rows, err := ur.Pools.LogsRead.QueryContext(req.Context(),
			"SELECT DISTINCT labels FROM metrics WHERE name = ? AND labels IS NOT NULL LIMIT 200", metric)
		if err == nil {
			defer rows.Close()
			seen := map[string]bool{}
			for rows.Next() {
				var raw string
				if rows.Scan(&raw) != nil {
					continue
				}
				var m map[string]any
				if json.Unmarshal([]byte(raw), &m) != nil {
					continue
				}
				for k := range m {
					if !seen[k] {
						seen[k] = true
						keys = append(keys, k)
					}
				}
			}
			sort.Strings(keys[2:])
		}
		respond(keys)
		return
	}

	var q string
	args := []any{metric}
	switch key {
	case "service":
		q = "SELECT DISTINCT service FROM metrics WHERE name = ? ORDER BY service LIMIT 50"
	case "host":
		q = "SELECT DISTINCT host FROM metrics WHERE name = ? ORDER BY host LIMIT 50"
	default:
		if !labelKeyRe.MatchString(key) {
			respond(nil)
			return
		}
		q = "SELECT DISTINCT json_extract(labels, '$.' || ?) AS v FROM metrics WHERE name = ? AND v IS NOT NULL ORDER BY v LIMIT 50"
		args = []any{key, metric}
	}
	rows, err := ur.Pools.LogsRead.QueryContext(req.Context(), q, args...)
	if err != nil {
		respond(nil)
		return
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var v string
		if rows.Scan(&v) == nil && v != "" {
			values = append(values, v)
		}
	}
	respond(values)
}

func (ur *Router) servicesForMetric(req *http.Request, metric string) []string {
	rows, err := ur.Pools.LogsRead.QueryContext(req.Context(),
		"SELECT DISTINCT service FROM metrics WHERE name = ? ORDER BY service LIMIT 7", metric)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var services []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			services = append(services, s)
		}
	}
	return services
}

func (ur *Router) metricsCatalog(req *http.Request) []templates.CatalogRow {
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead,
		"SELECT name, MIN(type) AS type, service, COUNT(*) AS points, MAX(timestamp) AS last_seen FROM metrics GROUP BY name, service ORDER BY last_seen DESC LIMIT 100")
	if err != nil {
		return nil
	}
	out := make([]templates.CatalogRow, 0, len(res.Rows))
	for _, row := range res.Rows {
		c := templates.CatalogRow{
			Name:       cellString(row[0]),
			Service:    cellString(row[2]),
			Points:     asInt64(row[3]),
			LastSeenMs: asInt64(row[4]),
		}
		if !strings.Contains(c.Name, ")") { // a ')' would break the DSL head
			if asInt64(row[1]) == 0 {
				c.DSL = "increase(" + c.Name + ") per 1h last 24h"
			} else {
				c.DSL = "avg(" + c.Name + ") per 10m last 24h"
			}
		}
		out = append(out, c)
	}
	return out
}

// metricNames feeds the DSL autocomplete: offering the catalog for the USER
// to pick from is not the name-guessing we rejected for snippets.
func (ur *Router) metricNames(req *http.Request) ([]string, error) {
	rows, err := ur.Pools.LogsRead.QueryContext(req.Context(),
		"SELECT DISTINCT name FROM metrics ORDER BY name LIMIT 500")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	return names, rows.Err()
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
	if strings.Contains(p.Query, snippetPlaceholderMarker) {
		// a placeholder panel would render 0 rows forever; catch it here
		ur.renderExplorer(w, req, p.Query, placeholderErr(p.Query))
		return
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
