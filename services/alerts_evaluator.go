package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/n0rdy/pooml/query"

	"github.com/rs/zerolog/log"
)

// AlertNotifier is what the evaluator needs from the notification layer;
// tests substitute a recorder.
type AlertNotifier interface {
	SendAlert(ctx context.Context, a Alert, res *query.Result) error
}

// Evaluator is the alert master loop from CONTEXT.md > Alerting: one ticker,
// per-due-alert goroutines, state in the DB so edits are picked up next tick.
type Evaluator struct {
	store    *AlertsService
	logsRead *sql.DB
	notifier AlertNotifier

	inflight sync.Map
	wg       sync.WaitGroup
}

func NewEvaluator(store *AlertsService, logsRead *sql.DB, notifier AlertNotifier) *Evaluator {
	return &Evaluator{store: store, logsRead: logsRead, notifier: notifier}
}

// Run blocks until ctx is cancelled, then waits for in-flight evaluations
// (each bounded by the query timeout and notifier timeout, so this converges).
func (e *Evaluator) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

func (e *Evaluator) tick(ctx context.Context) {
	alerts, err := e.store.ListEnabled(ctx)
	if err != nil {
		log.Error().Err(err).Msg("alert evaluator: list")
		return
	}
	now := time.Now().UnixMilli()
	for _, a := range alerts {
		if a.LastCheckedAt.Valid && now-a.LastCheckedAt.Int64 < a.CheckIntervalMs {
			continue
		}
		if _, busy := e.inflight.LoadOrStore(a.ID, struct{}{}); busy {
			continue
		}
		e.wg.Add(1)
		go func(a Alert) {
			defer e.wg.Done()
			defer e.inflight.Delete(a.ID)
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Int64("alert", a.ID).Msg("alert evaluation panicked")
				}
			}()
			e.evaluate(ctx, a)
		}(a)
	}
}

func (e *Evaluator) evaluate(ctx context.Context, a Alert) {
	now := time.Now().UnixMilli()

	v, err := query.Validate(a.Query)
	if err != nil {
		e.recordError(ctx, a, err.Error(), now)
		return
	}
	res, err := query.Execute(ctx, e.logsRead, v.SQL())
	if err != nil {
		e.recordError(ctx, a, "query failed: "+err.Error(), now)
		return
	}

	firing := len(res.Rows) > 0
	if !firing {
		if err := e.store.UpdateEval(ctx, a.ID, false, nil, nil, now); err != nil {
			log.Error().Err(err).Int64("alert", a.ID).Msg("alert state update")
		}
		return
	}

	// level-triggered + cooldown: currently_firing reflects reality even while
	// notifications are suppressed; self-healing re-notify after cooldown
	cooldownPassed := !a.LastFiredAt.Valid || now-a.LastFiredAt.Int64 >= a.CooldownMs
	if !cooldownPassed {
		if err := e.store.UpdateEval(ctx, a.ID, true, nil, nil, now); err != nil {
			log.Error().Err(err).Int64("alert", a.ID).Msg("alert state update")
		}
		return
	}

	notifyErr := e.notifier.SendAlert(ctx, a, res)
	matched, _ := json.Marshal(matchedRowsPayload(res))
	if err := e.store.RecordFiring(ctx, a.ID, now, string(matched), notifyErr == nil); err != nil {
		log.Error().Err(err).Int64("alert", a.ID).Msg("alert firing audit")
	}

	if notifyErr != nil {
		// last_fired_at deliberately NOT advanced: next eligible cycle retries
		msg := "notification failed: " + notifyErr.Error()
		if err := e.store.UpdateEval(ctx, a.ID, true, nil, &msg, now); err != nil {
			log.Error().Err(err).Int64("alert", a.ID).Msg("alert state update")
		}
		return
	}
	if err := e.store.UpdateEval(ctx, a.ID, true, &now, nil, now); err != nil {
		log.Error().Err(err).Int64("alert", a.ID).Msg("alert state update")
	}
}

func (e *Evaluator) recordError(ctx context.Context, a Alert, msg string, now int64) {
	if err := e.store.UpdateEval(ctx, a.ID, a.CurrentlyFiring, nil, &msg, now); err != nil {
		log.Error().Err(err).Int64("alert", a.ID).Msg("alert state update")
	}
}

const (
	auditMaxRows     = 20
	auditMaxValueLen = 200
)

func matchedRowsPayload(res *query.Result) []map[string]any {
	n := min(len(res.Rows), auditMaxRows)
	out := make([]map[string]any, 0, n)
	for _, row := range res.Rows[:n] {
		m := make(map[string]any, len(res.Columns))
		for i, c := range res.Columns {
			if i >= len(row) {
				continue
			}
			// raw log lines can approach the 2 MB ingest cap; the audit
			// trail wants the gist, not meta.db bloat
			if s, ok := row[i].(string); ok && len(s) > auditMaxValueLen {
				m[c] = truncateAtRune(s, auditMaxValueLen) + "…"
			} else {
				m[c] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}
