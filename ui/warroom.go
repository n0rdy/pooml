package ui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

const (
	warMetricRowCount = 3
	warDefaultSpanMs  = int64(60 * 60 * 1000)
	warMinSpanMs      = int64(60 * 1000)
)

type warRequest struct {
	fromMs, toMs int64
	follow       bool
	dsls         []string
	q, fts, op   string
}

func parseWarRequest(req *http.Request) warRequest {
	qp := req.URL.Query()
	now := time.Now().UnixMilli()

	wr := warRequest{
		follow: qp.Get("follow") == "1",
		q:      strings.TrimSpace(qp.Get("q")),
		fts:    strings.TrimSpace(qp.Get("fts")),
		op:     qp.Get("op"),
	}
	if wr.q == "" {
		wr.q = templates.WarDefaultLogsQuery
	}
	if wr.op != "or" {
		wr.op = "and"
	}

	from, _ := strconv.ParseInt(qp.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(qp.Get("to"), 10, 64)
	span := to - from
	if from <= 0 || to <= 0 || span < warMinSpanMs {
		span = warDefaultSpanMs
		to = now
		from = now - span
	}
	// follow keeps the span but pins the right edge to now, every render.
	// A future right edge is otherwise legal (alert links center on firing).
	if wr.follow {
		to = now
		from = now - span
	}
	wr.fromMs, wr.toMs = from, to

	// filled rows first: positions are display slots, not identity
	wr.dsls = make([]string, 0, warMetricRowCount)
	for _, dsl := range qp["dsl"] {
		if dsl = strings.TrimSpace(dsl); dsl != "" && len(wr.dsls) < warMetricRowCount {
			wr.dsls = append(wr.dsls, dsl)
		}
	}
	for len(wr.dsls) < warMetricRowCount {
		wr.dsls = append(wr.dsls, "")
	}
	return wr
}

// GET /war-room - the time-locked split view (M12). All state lives in the
// URL: from/to/follow, up to 3 dsl metric rows, q/fts/op for the logs pane.
// ?frag=panes returns only the OOB pane fragments for the follow refresh.
func (ur *Router) warRoomPage(w http.ResponseWriter, req *http.Request) {
	wr := parseWarRequest(req)
	view := templates.WarRoomView{FromMs: wr.fromMs, ToMs: wr.toMs, Follow: wr.follow}
	if names, err := ur.metricNames(req); err == nil {
		view.MetricNames = names
	}
	for i, dsl := range wr.dsls {
		view.Metrics = append(view.Metrics, ur.warMetricRow(req, i, dsl, wr.fromMs, wr.toMs))
	}
	view.MiniChart, view.MiniTotal, view.MiniErrors = ur.warMiniChart(req, wr.fromMs, wr.toMs)
	ur.warLogsPane(req, wr, &view)

	if req.URL.Query().Get("frag") == "panes" {
		ur.render(w, req, http.StatusOK, templates.WarRoomPanes(view))
		return
	}
	ur.render(w, req, http.StatusOK, templates.WarRoomPage(view, nosurf.Token(req)))
}

// warMetricRow compiles one DSL row with the page window overriding its
// `last` clause, mirroring the explorer's server-side authority rule.
func (ur *Router) warMetricRow(req *http.Request, i int, dsl string, fromMs, toMs int64) templates.WarMetricRow {
	row := templates.WarMetricRow{Index: i, DSL: dsl}
	if dsl == "" {
		return row
	}
	d, err := query.ParseDSL(dsl)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	d.OverrideWindow(fromMs, toMs)
	var services []string
	if d.NeedsServices() {
		services = ur.servicesForMetric(req, d.Metric)
	}
	sql, err := d.SQL(services)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	v, err := query.ValidateIn(sql, query.ScopeMetrics)
	if err != nil {
		row.Err = humanizeSQLError(err)
		return row
	}
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL())
	if err != nil {
		row.Err = err.Error()
		return row
	}
	var fill *chartFill
	if d.BucketMs > 0 && d.Verb != "latest" {
		fill = &chartFill{
			BucketMs: d.BucketMs,
			StartMs:  fromMs / d.BucketMs * d.BucketMs,
			EndMs:    toMs / d.BucketMs * d.BucketMs,
			Zero:     d.Verb == "increase" || d.Verb == "rate" || d.Verb == "count",
		}
	}
	row.Shaped = shapeResult(res, "", fmt.Sprintf("w%d", i), fill)
	return row
}

// warBucketSteps: mini-chart bucket sizes, picked so the window renders as
// roughly 60-120 bars.
var warBucketSteps = []int64{
	1000, 5000, 15000, 30000, 60000, 5 * 60000, 10 * 60000, 30 * 60000,
	3600000, 3 * 3600000, 6 * 3600000, 12 * 3600000, 86400000,
}

func warBucketMs(spanMs int64) int64 {
	for _, s := range warBucketSteps {
		if spanMs/s <= 120 {
			return s
		}
	}
	// beyond ~120 days: whole days, scaled to fit
	return (spanMs/120/86400000 + 1) * 86400000
}

// warMiniChart is the fixed internal volume/error query - not user input,
// same policy as the home fragments.
func (ur *Router) warMiniChart(req *http.Request, fromMs, toMs int64) (*templates.ChartPayload, int64, int64) {
	b := warBucketMs(toMs - fromMs)
	rows, err := ur.Pools.LogsRead.QueryContext(req.Context(),
		"SELECT (timestamp / ?) * ? AS bucket, COUNT(*), SUM(CASE WHEN level >= ? THEN 1 ELSE 0 END) FROM logs WHERE timestamp BETWEEN ? AND ? GROUP BY bucket",
		b, b, common.LevelError, fromMs, toMs)
	if err != nil {
		log.Error().Err(err).Msg("war room volume query")
		return nil, 0, 0
	}
	defer rows.Close()

	type bucketCounts struct{ all, errs int64 }
	counts := make(map[int64]bucketCounts)
	for rows.Next() {
		var bucket int64
		var v bucketCounts
		if err := rows.Scan(&bucket, &v.all, &v.errs); err != nil {
			return nil, 0, 0
		}
		counts[bucket] = v
	}
	if rows.Err() != nil {
		return nil, 0, 0
	}

	var labels []int64
	var allData, errData []*float64
	var total, errTotal int64
	for t := fromMs / b * b; t <= toMs; t += b {
		v := counts[t]
		all, errs := float64(v.all), float64(v.errs)
		labels = append(labels, t)
		allData = append(allData, &all)
		errData = append(errData, &errs)
		total += v.all
		errTotal += v.errs
	}
	payload := &templates.ChartPayload{
		Type:     "bar",
		LabelsMs: labels,
		Datasets: []templates.ChartDataset{
			{Label: "logs", Data: allData},
			{Label: "errors", Data: errData, Color: "#ef4444"},
		},
	}
	return payload, total, errTotal
}

// warLogsPane runs the logs pane query with the window's BETWEEN merged in at
// execution only - the URL's q stays window-free by design.
func (ur *Router) warLogsPane(req *http.Request, wr warRequest, view *templates.WarRoomView) {
	view.Logs = templates.LogsView{Q: wr.q, FTS: wr.fts, Op: wr.op}

	if strings.Contains(wr.q, snippetPlaceholderMarker) {
		view.Logs.Error = placeholderErr(wr.q)
		return
	}
	v, err := query.ValidateIn(wr.q, query.ScopeLogs)
	if err != nil {
		view.Logs.Error = humanizeSQLError(err)
		return
	}
	var args []any
	if wr.fts != "" {
		if err := v.CombineFTS(wr.op); err != nil {
			view.Logs.Error = "Full-text search doesn't combine with aggregation queries. Write the FTS join into the SQL instead."
			return
		}
		args = append(args, query.FTSMatch(wr.fts))
	}
	if err := v.ApplyWindow(wr.fromMs, wr.toMs); err != nil {
		view.Logs.Error = "The time window doesn't combine with compound (UNION) queries. Drop the UNION, or run this on the Logs page."
		return
	}
	res, err := query.Execute(req.Context(), ur.Pools.LogsRead, v.SQL(), args...)
	if err != nil {
		view.Logs.Error = "Query failed: " + err.Error()
		return
	}
	fillResults(&view.Logs, res)
	view.Logs.HasMore = false // no pagination here; the window bounds the set
}
