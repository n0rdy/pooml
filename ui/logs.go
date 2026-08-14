package ui

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/go-chi/chi/v5"
	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

const (
	defaultLogsQuery = "SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100"
	logsPageSize     = 50
)

// logViewerColumns is the exact result shape that renders as the log viewer;
// anything else renders as a generic table.
var logViewerColumns = []string{"id", "timestamp", "ingested_at", "level", "service", "host", "message", "parsed", "raw"}

type logsRequest struct {
	Q   string
	FTS string
	Op  string
}

func parseLogsRequest(req *http.Request) logsRequest {
	qp := req.URL.Query()
	r := logsRequest{
		Q:   strings.TrimSpace(qp.Get("q")),
		FTS: strings.TrimSpace(qp.Get("fts")),
		Op:  qp.Get("op"),
	}
	if r.Q == "" {
		r.Q = defaultLogsQuery
	}
	if r.Op != "or" {
		r.Op = "and"
	}
	return r
}

// GET /logs - full page, results fragment (HX-Request), or pagination rows
// fragment (before param). See CONTEXT.md > Querying.
func (ur *Router) logsPage(w http.ResponseWriter, req *http.Request) {
	lr := parseLogsRequest(req)
	qp := req.URL.Query()

	view := templates.LogsView{Q: lr.Q, FTS: lr.FTS, Op: lr.Op}

	v, err := query.ValidateIn(lr.Q, query.ScopeLogs)
	if err != nil {
		view.Error = humanizeSQLError(err)
		if line, col, msg, ok := sqlErrorPosition(err); ok {
			payload, _ := json.Marshal(map[string]any{
				"sqlError": map[string]any{"line": line, "col": col, "message": msg},
			})
			w.Header().Set("HX-Trigger", string(payload))
		}
		ur.renderLogs(w, req, view, false)
		return
	}

	// quick filters merge into q; the merged form becomes the canonical query
	filterApplied := false
	for _, col := range []string{"service", "level", "host"} {
		if val := qp.Get(col); val != "" {
			if err := v.ApplyQuickFilter(col, val); err != nil {
				view.Error = "This filter doesn't fit the current query shape. Refine the SQL by hand instead."
				ur.renderLogs(w, req, view, false)
				return
			}
			filterApplied = true
		}
	}
	view.Q = v.SQL()

	var args []any
	if lr.FTS != "" {
		if err := v.CombineFTS(lr.Op); err != nil {
			view.Error = "Full-text search doesn't combine with aggregation queries. Write the FTS join into the SQL instead."
			ur.renderLogs(w, req, view, false)
			return
		}
		args = append(args, query.FTSMatch(lr.FTS))
	}

	isPageFetch := false
	if beforeTs := qp.Get("before_ts"); beforeTs != "" {
		ts, tsErr := strconv.ParseInt(beforeTs, 10, 64)
		id, idErr := strconv.ParseInt(qp.Get("before_id"), 10, 64)
		if tsErr != nil || idErr != nil || v.ApplyPagination(ts, id, logsPageSize) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		isPageFetch = true
	}

	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL(), args...)
	if err != nil {
		view.Error = "Query failed: " + err.Error()
		ur.renderLogs(w, req, view, false)
		return
	}
	fillResults(&view, res)
	view.AtStart = isPageFetch && len(view.Rows) == 0

	if filterApplied {
		// canonical state after a quick filter is ?q={merged}; the editor
		// follows via the setQuery event
		payload, _ := json.Marshal(map[string]map[string]string{"setQuery": {"q": view.Q}})
		w.Header().Set("HX-Trigger", string(payload))
		w.Header().Set("HX-Push-Url", "/logs?q="+url.QueryEscape(view.Q))
	}
	ur.renderLogs(w, req, view, isPageFetch)
}

func (ur *Router) renderLogs(w http.ResponseWriter, req *http.Request, view templates.LogsView, isPageFetch bool) {
	switch {
	case isPageFetch:
		ur.render(w, req, http.StatusOK, templates.LogRowsPage(view))
	case req.Header.Get("HX-Request") == "true" && req.Header.Get("HX-History-Restore-Request") != "true":
		ur.render(w, req, http.StatusOK, templates.LogsResults(view))
	default:
		ur.render(w, req, http.StatusOK, templates.LogsPage(view, nosurf.Token(req)))
	}
}

// fillResults classifies the result shape and builds the view model.
// Log-viewer rows are displayed oldest-to-newest (newest at bottom, like
// tail -f) regardless of the query's ORDER BY, which only governs selection.
func fillResults(view *templates.LogsView, res *query.Result) {
	view.Truncated = res.Truncated

	colIdx := make(map[string]int, len(res.Columns))
	for i, c := range res.Columns {
		colIdx[c] = i
	}
	isViewer := len(res.Columns) == len(logViewerColumns)
	for _, c := range logViewerColumns {
		if _, ok := colIdx[c]; !ok {
			isViewer = false
			break
		}
	}

	if !isViewer {
		view.Columns = res.Columns
		view.Cells = make([][]string, len(res.Rows))
		for i, row := range res.Rows {
			cells := make([]string, len(row))
			for j, cell := range row {
				cells[j] = cellString(cell)
			}
			view.Cells[i] = cells
		}
		return
	}

	view.IsViewer = true
	view.Rows = make([]templates.LogRow, 0, len(res.Rows))
	for _, row := range res.Rows {
		lr := templates.LogRow{
			ID:      asInt64(row[colIdx["id"]]),
			Ts:      asInt64(row[colIdx["timestamp"]]),
			Service: cellString(row[colIdx["service"]]),
			Host:    cellString(row[colIdx["host"]]),
			Raw:     cellString(row[colIdx["raw"]]),
		}
		if lv := row[colIdx["level"]]; lv != nil {
			n := int(asInt64(lv))
			lr.Level = &n
		}
		if msg := row[colIdx["message"]]; msg != nil {
			lr.Message = cellString(msg)
		} else {
			lr.Message = lr.Raw
		}
		view.Rows = append(view.Rows, lr)
	}
	// event-time order, id as tiebreak: the viewer contract is chronological
	// regardless of ingestion order (CLF embedded times, backfills)
	sort.Slice(view.Rows, func(i, j int) bool {
		if view.Rows[i].Ts != view.Rows[j].Ts {
			return view.Rows[i].Ts < view.Rows[j].Ts
		}
		return view.Rows[i].ID < view.Rows[j].ID
	})

	if len(view.Rows) > 0 {
		// always offer the sentinel; the probe that comes back empty renders
		// the beginning-of-history marker instead. One extra request at the
		// top of history beats guessing against the query's own LIMIT.
		view.NextBeforeTs = view.Rows[0].Ts
		view.NextBeforeID = view.Rows[0].ID
		view.HasMore = true
	}
}

// rowFromResult builds a viewer row from an executed result row; ok is false
// when the projection lacks the full logs.* column set.
func rowFromResult(colIdx map[string]int, row []any) (templates.LogRow, bool) {
	for _, c := range logViewerColumns {
		if _, ok := colIdx[c]; !ok {
			return templates.LogRow{}, false
		}
	}
	r := templates.LogRow{
		ID:      asInt64(row[colIdx["id"]]),
		Ts:      asInt64(row[colIdx["timestamp"]]),
		Service: cellString(row[colIdx["service"]]),
		Host:    cellString(row[colIdx["host"]]),
		Raw:     cellString(row[colIdx["raw"]]),
	}
	if lv := row[colIdx["level"]]; lv != nil {
		n := int(asInt64(lv))
		r.Level = &n
	}
	if msg := row[colIdx["message"]]; msg != nil {
		r.Message = cellString(msg)
	} else {
		r.Message = r.Raw
	}
	return r, true
}

// GET /logs/{id} - expanded detail fragment for one log row.
func (ur *Router) logDetailsPage(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var d templates.LogDetail
	var level, message, parsed any
	err = ur.Pools.LogsRead.QueryRowContext(req.Context(),
		"SELECT id, timestamp, ingested_at, level, service, host, message, parsed, raw FROM logs WHERE id = ?", id).
		Scan(&d.ID, &d.Ts, &d.IngestedAt, &level, &d.Service, &d.Host, &message, &parsed, &d.Raw)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if level != nil {
		n := int(asInt64(level))
		d.Level = &n
	}
	if parsed != nil {
		d.Parsed = prettyJSON(cellString(parsed))
	}
	ur.render(w, req, http.StatusOK, templates.LogDetailRow(d))
}

// GET /logs/export?format=csv|json - current results as a download.
func (ur *Router) exportLogs(w http.ResponseWriter, req *http.Request) {
	lr := parseLogsRequest(req)

	v, err := query.ValidateIn(lr.Q, query.ScopeLogs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var args []any
	if lr.FTS != "" {
		if err := v.CombineFTS(lr.Op); err == nil {
			args = append(args, query.FTSMatch(lr.FTS))
		}
	}
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL(), args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="logs.json"`)
		out := make([]map[string]any, len(res.Rows))
		for i, row := range res.Rows {
			m := make(map[string]any, len(res.Columns))
			for j, col := range res.Columns {
				m[col] = row[j]
			}
			out[i] = m
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Error().Err(err).Msg("json export")
		}
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.csv"`)
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
		log.Error().Err(err).Msg("csv export")
	}
}

// rqlite parse errors look like `invalid SQL: 3:27: expected semicolon or
// EOF, found 100` - precise, but hostile as user-facing text.
var sqlErrPosRe = regexp.MustCompile(`(\d+):(\d+): (.+)$`)

var sqlErrTranslations = [][2]string{
	{"expected semicolon or EOF, found", "the query should end here, but found"},
	{"expected expression, found", "an expression is missing; found"},
}

func sqlErrorPosition(err error) (line, col int, msg string, ok bool) {
	m := sqlErrPosRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, 0, "", false
	}
	line, _ = strconv.Atoi(m[1])
	col, _ = strconv.Atoi(m[2])
	msg = m[3]
	for _, tr := range sqlErrTranslations {
		msg = strings.ReplaceAll(msg, tr[0], tr[1])
	}
	return line, col, msg, true
}

func humanizeSQLError(err error) string {
	if line, col, msg, ok := sqlErrorPosition(err); ok {
		return fmt.Sprintf("Line %d, col %d: %s", line, col, msg)
	}
	return err.Error()
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(pretty)
}
