package ui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/query"
	"github.com/n0rdy/pooml/ui/templates"

	"github.com/a-h/templ"
	"github.com/rs/zerolog/log"
)

// Tunable so tests can shrink them; see SetStreamIntervalsForTest.
var (
	streamPollInterval      = 1 * time.Second
	streamRefreshInterval   = 5 * time.Second
	streamHeartbeatInterval = 15 * time.Second
)

const (
	streamFlushInterval = 100 * time.Millisecond
	streamMaxBatch      = 50
	streamPollLimit     = 100
)

// GET /logs/stream - SSE live tail. Strategy by query shape (see CONTEXT.md >
// Streaming): A broadcaster fast path for unfiltered viewer queries, B cursor
// polling for filtered viewer queries, C periodic full refresh for
// aggregations. No backfill: the page render already shows current state.
func (ur *Router) streamLogs(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lr := parseLogsRequest(req)

	v, err := query.ValidateIn(lr.Q, query.ScopeLogs)
	if err != nil {
		http.Error(w, humanizeSQLError(err), http.StatusBadRequest)
		return
	}
	if v.Compound {
		http.Error(w, "live tail is not supported for compound queries", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sw := &sseWriter{w: w, f: flusher}

	switch {
	case v.Shape == query.ShapeLogViewer && lr.FTS == "" && !v.HasConditions():
		ur.streamBroadcast(req, sw, lr.Q)
	case v.Shape == query.ShapeLogViewer:
		ur.streamPolling(req, sw, lr, v)
	default:
		ur.streamRefresh(req, sw, lr, v)
	}
}

// Strategy A: subscribe to the ingestion broadcaster; zero DB work.
// Streamed rows have no id yet (broadcast happens before persistence), so
// they render without data-id and the client skips detail expansion on them.
func (ur *Router) streamBroadcast(req *http.Request, sw *sseWriter, q string) {
	ch, unsub := ur.Broadcaster.Subscribe()
	defer unsub()

	flushTicker := time.NewTicker(streamFlushInterval)
	defer flushTicker.Stop()
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-ur.StreamCtx.Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			html, err := renderComponent(req.Context(), templates.StreamRow(standardToRow(l), q))
			if err != nil {
				log.Error().Err(err).Msg("stream row render")
				continue
			}
			sw.event("log", html)
			if sw.pending >= streamMaxBatch {
				sw.flush()
			}
		case <-flushTicker.C:
			sw.flush()
		case <-heartbeat.C:
			sw.comment("ping")
			sw.flush()
		}
	}
}

// Strategy B: cursor polling on the read pool. The cursor query is built once
// with a bind param; each tick executes it with the latest anchor. If the
// user's SELECT doesn't produce the full log-viewer column set, falls back to
// strategy C semantics via the caller-provided original query.
func (ur *Router) streamPolling(req *http.Request, sw *sseWriter, lr logsRequest, v *query.Validated) {
	var args []any
	if lr.FTS != "" {
		if err := v.CombineFTS(lr.Op); err != nil {
			sw.event("streamerror", "full-text search does not combine with this query shape")
			sw.flush()
			return
		}
		args = append(args, query.FTSMatch(lr.FTS))
	}
	if err := v.ApplyStreamCursor(streamPollLimit); err != nil {
		sw.event("streamerror", "live tail is not supported for this query shape")
		sw.flush()
		return
	}
	sqlText := v.SQL()

	var lastID int64
	if err := ur.Pools.LogsRead.QueryRowContext(req.Context(),
		"SELECT COALESCE(MAX(id), 0) FROM logs").Scan(&lastID); err != nil {
		log.Error().Err(err).Msg("stream anchor")
		return
	}

	poll := time.NewTicker(streamPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-ur.StreamCtx.Done():
			return
		case <-heartbeat.C:
			sw.comment("ping")
			sw.flush()
		case <-poll.C:
			res, err := query.Execute(req.Context(), ur.Pools.LogsRead, sqlText, append(args, lastID)...)
			if err != nil {
				log.Error().Err(err).Msg("stream poll")
				continue
			}
			colIdx := make(map[string]int, len(res.Columns))
			for i, c := range res.Columns {
				colIdx[c] = i
			}
			for _, row := range res.Rows {
				r, ok := rowFromResult(colIdx, row)
				if !ok {
					sw.event("streamerror", "live tail needs the full logs.* column set; showing periodic refresh is not available for this projection")
					sw.flush()
					return
				}
				if r.ID > lastID {
					lastID = r.ID
				}
				html, err := renderComponent(req.Context(), templates.StreamRow(r, lr.Q))
				if err != nil {
					log.Error().Err(err).Msg("stream row render")
					continue
				}
				sw.event("log", html)
			}
			sw.flush()
		}
	}
}

// Strategy C: re-run the query periodically, ship the whole results fragment.
func (ur *Router) streamRefresh(req *http.Request, sw *sseWriter, lr logsRequest, v *query.Validated) {
	var args []any
	if lr.FTS != "" {
		if err := v.CombineFTS(lr.Op); err != nil {
			// refreshing UNFILTERED results here would silently show the
			// user different data than they asked for
			sw.event("streamerror", "full-text search does not combine with this query shape")
			sw.flush()
			return
		}
		args = append(args, query.FTSMatch(lr.FTS))
	}
	sqlText := v.SQL()

	refresh := time.NewTicker(streamRefreshInterval)
	defer refresh.Stop()
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-ur.StreamCtx.Done():
			return
		case <-heartbeat.C:
			sw.comment("ping")
			sw.flush()
		case <-refresh.C:
			res, err := query.Execute(req.Context(), ur.Pools.LogsRead, sqlText, args...)
			if err != nil {
				log.Error().Err(err).Msg("stream refresh")
				continue
			}
			view := templates.LogsView{Q: lr.Q, FTS: lr.FTS, Op: lr.Op}
			fillResults(&view, res)
			html, err := renderComponent(req.Context(), templates.LogsResults(view))
			if err != nil {
				log.Error().Err(err).Msg("stream refresh render")
				continue
			}
			sw.event("refresh", html)
			sw.flush()
		}
	}
}

// sseWriter frames SSE events; flushing is the caller's batching decision.
type sseWriter struct {
	w       http.ResponseWriter
	f       http.Flusher
	pending int
}

func (s *sseWriter) event(name, data string) {
	fmt.Fprintf(s.w, "event: %s\n", name)
	// SSE treats a bare \r as a line terminator too: an unstripped CR in log
	// content would break out of the data field and spoof events
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r", ""), "\n") {
		fmt.Fprintf(s.w, "data: %s\n", line)
	}
	fmt.Fprint(s.w, "\n")
	s.pending++
}

func (s *sseWriter) comment(text string) {
	fmt.Fprintf(s.w, ": %s\n\n", text)
	s.pending++
}

func (s *sseWriter) flush() {
	if s.pending > 0 {
		s.f.Flush()
		s.pending = 0
	}
}

func renderComponent(ctx context.Context, c templ.Component) (string, error) {
	var b strings.Builder
	err := c.Render(ctx, &b)
	return b.String(), err
}

func standardToRow(l common.StandardLog) templates.LogRow {
	r := templates.LogRow{
		Ts:      l.Timestamp,
		Level:   l.Level,
		Service: l.Service,
		Host:    l.Host,
		Raw:     l.Raw,
		Message: l.Raw,
	}
	if l.Message != nil {
		r.Message = *l.Message
	}
	return r
}
