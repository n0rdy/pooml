package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/query"
)

// QueryAPI is the read-only SQL-over-HTTP surface (M11a). Deliberately
// gated like /metrics: disabled by default, own env secret (never an ingest
// API key - a leaked ingest key must not grant read access to log data),
// route not registered when off. See CONTEXT.md > Query API.
type QueryAPI struct {
	Secret   string // empty = disabled
	LogsRead *sql.DB
	Metrics  *sql.DB
}

const (
	// budgets tuned for programmatic consumers (LLM context windows,
	// scripts): far tighter than the UI's 10K row cap
	queryDefaultMaxRows = 200
	queryMaxMaxRows     = 1000
	queryMaxCellBytes   = 4096
	queryMaxBodyBytes   = 64 << 10
)

type queryRequest struct {
	SQL     string `json:"sql"`
	MaxRows int    `json:"max_rows"`
}

type queryResponse struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

func (ar *Router) queryLogs(w http.ResponseWriter, req *http.Request) {
	ar.runQuery(w, req, query.ScopeLogs, ar.queryAPI.LogsRead)
}

func (ar *Router) queryMetrics(w http.ResponseWriter, req *http.Request) {
	ar.runQuery(w, req, query.ScopeMetrics, ar.queryAPI.Metrics)
}

func (ar *Router) runQuery(w http.ResponseWriter, req *http.Request, scope query.Scope, db *sql.DB) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, queryMaxBodyBytes))
	if err != nil {
		ar.sendQueryError(w, http.StatusBadRequest, "request body unreadable or over 64KB")
		return
	}
	var q queryRequest
	if err := json.Unmarshal(body, &q); err != nil {
		ar.sendQueryError(w, http.StatusBadRequest, "body must be JSON: {\"sql\": \"...\", \"max_rows\": 200}")
		return
	}
	if q.SQL == "" {
		ar.sendQueryError(w, http.StatusBadRequest, "sql is required")
		return
	}

	v, err := query.ValidateIn(q.SQL, scope)
	if err != nil {
		ar.sendQueryError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := query.Execute(req.Context(), db, v.SQL())
	if err != nil {
		ar.sendQueryError(w, http.StatusBadRequest, err.Error())
		return
	}

	maxRows := q.MaxRows
	if maxRows <= 0 {
		maxRows = queryDefaultMaxRows
	}
	if maxRows > queryMaxMaxRows {
		maxRows = queryMaxMaxRows
	}
	truncated := res.Truncated
	rows := res.Rows
	if len(rows) > maxRows {
		rows, truncated = rows[:maxRows], true
	}
	out := make([][]any, len(rows))
	for i, row := range rows {
		cells := make([]any, len(row))
		for j, cell := range row {
			cells[j] = sanitizeCell(cell)
		}
		out[i] = cells
	}
	ar.sendJsonResponse(w, http.StatusOK, queryResponse{
		Columns:   res.Columns,
		Rows:      out,
		RowCount:  len(out),
		Truncated: truncated,
	})
}

// GET /api/v1/query/catalog - what metrics exist, so a consumer can orient
// without guessing names.
func (ar *Router) queryCatalog(w http.ResponseWriter, req *http.Request) {
	rows, err := ar.queryAPI.Metrics.QueryContext(req.Context(),
		"SELECT name, MIN(type) AS type, service, COUNT(*) AS points, MAX(timestamp) AS last_seen FROM metrics GROUP BY name, service ORDER BY last_seen DESC LIMIT 200")
	if err != nil {
		ar.sendErrorResponse(w, http.StatusInternalServerError, common.ErrCodeInternal)
		return
	}
	defer rows.Close()

	type catalogRow struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Service    string `json:"service"`
		Points     int64  `json:"points"`
		LastSeenMs int64  `json:"last_seen_ms"`
	}
	out := []catalogRow{}
	for rows.Next() {
		var r catalogRow
		var typ int
		if err := rows.Scan(&r.Name, &typ, &r.Service, &r.Points, &r.LastSeenMs); err != nil {
			ar.sendErrorResponse(w, http.StatusInternalServerError, common.ErrCodeInternal)
			return
		}
		r.Type = "counter"
		if typ == 1 {
			r.Type = "gauge"
		}
		out = append(out, r)
	}
	ar.sendJsonResponse(w, http.StatusOK, out)
}

// sanitizeCell makes driver values JSON-friendly and bounds their size: a
// single raw log line can approach the 2 MB ingest cap, which no
// programmatic consumer wants in a result row.
func sanitizeCell(v any) any {
	switch t := v.(type) {
	case []byte:
		return clampString(string(t))
	case string:
		return clampString(t)
	default:
		return v
	}
}

func clampString(s string) string {
	if len(s) <= queryMaxCellBytes {
		return s
	}
	cut := queryMaxCellBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

func (ar *Router) sendQueryError(w http.ResponseWriter, code int, msg string) {
	ar.sendJsonResponse(w, code, common.ErrorResponse{Code: common.ErrCodeBadRequest, Message: msg})
}
