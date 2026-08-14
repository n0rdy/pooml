package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/n0rdy/pooml/metrics"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/rs/zerolog/log"
)

const (
	scrapeTimeout = 10 * time.Second
	// node_exporter with everything on emits ~2 MB; 10 MB flags a target that
	// is not really a metrics endpoint
	maxScrapeBodyBytes = 10 << 20
)

// MetricsPusher is what the scraper needs from the metrics pipeline; tests
// substitute a recorder.
type MetricsPusher interface {
	TryPush(rows []metrics.Row) bool
}

// Scraper is the Prometheus pull loop: same master-loop shape as the alert
// evaluator (1s tick, per-due-target goroutines, state in the DB so UI edits
// are picked up next tick). See CONTEXT.md > Metrics.
type Scraper struct {
	store    *ScrapeTargetsService
	pipeline MetricsPusher
	client   *http.Client

	inflight       sync.Map
	wg             sync.WaitGroup
	downcastWarned sync.Map
}

func NewScraper(store *ScrapeTargetsService, pipeline MetricsPusher) *Scraper {
	return &Scraper{
		store:    store,
		pipeline: pipeline,
		client:   &http.Client{Timeout: scrapeTimeout},
	}
}

func (s *Scraper) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scraper) tick(ctx context.Context) {
	targets, err := s.store.ListEnabled(ctx)
	if err != nil {
		log.Error().Err(err).Msg("scraper: list targets")
		return
	}
	now := time.Now().UnixMilli()
	for _, t := range targets {
		if t.LastScrapedAt.Valid && now-t.LastScrapedAt.Int64 < t.ScrapeIntervalMs {
			continue
		}
		if _, busy := s.inflight.LoadOrStore(t.ID, struct{}{}); busy {
			continue
		}
		s.wg.Add(1)
		go func(t ScrapeTarget) {
			defer s.wg.Done()
			defer s.inflight.Delete(t.ID)
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Int64("target", t.ID).Msg("scrape panicked")
				}
			}()
			s.scrape(ctx, t)
		}(t)
	}
}

func (s *Scraper) scrape(ctx context.Context, t ScrapeTarget) {
	now := time.Now().UnixMilli()
	rows, err := s.fetch(ctx, t, now)
	if err == nil && !s.pipeline.TryPush(rows) {
		err = fmt.Errorf("metrics pipeline saturated, scrape dropped")
	}

	var errMsg *string
	if err != nil {
		msg := err.Error()
		errMsg = &msg
		log.Warn().Err(err).Int64("target", t.ID).Str("url", t.URL).Msg("scrape failed")
	}
	if recErr := s.store.RecordScrape(ctx, t.ID, now, errMsg); recErr != nil {
		log.Error().Err(recErr).Int64("target", t.ID).Msg("scrape state update")
	}
}

func (s *Scraper) fetch(ctx context.Context, t ScrapeTarget, now int64) ([]metrics.Row, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return nil, err
	}
	if t.AuthHeader != "" {
		name, value, _ := strings.Cut(t.AuthHeader, ":")
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(io.LimitReader(resp.Body, maxScrapeBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return s.familiesToRows(families, t, now), nil
}

func (s *Scraper) familiesToRows(families map[string]*dto.MetricFamily, t ScrapeTarget, now int64) []metrics.Row {
	var rows []metrics.Row
	base := metrics.Row{Service: t.Service, Host: t.Host}

	appendRow := func(m *dto.Metric, name string, typ int, value float64) {
		r := base
		r.Name, r.Type, r.Value = name, typ, value
		r.Timestamp = now
		if m.GetTimestampMs() != 0 {
			r.Timestamp = m.GetTimestampMs()
		}
		r.Labels = promLabelsJSON(m.GetLabel())
		rows = append(rows, r)
	}

	for name, fam := range families {
		for _, m := range fam.GetMetric() {
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				appendRow(m, name, metrics.TypeCounter, m.GetCounter().GetValue())
			case dto.MetricType_GAUGE:
				appendRow(m, name, metrics.TypeGauge, m.GetGauge().GetValue())
			case dto.MetricType_UNTYPED:
				appendRow(m, name, metrics.TypeGauge, m.GetUntyped().GetValue())
			case dto.MetricType_HISTOGRAM:
				s.warnDowncast(name, "histogram")
				appendRow(m, name+"_sum", metrics.TypeGauge, m.GetHistogram().GetSampleSum())
				appendRow(m, name+"_count", metrics.TypeGauge, float64(m.GetHistogram().GetSampleCount()))
			case dto.MetricType_SUMMARY:
				s.warnDowncast(name, "summary")
				appendRow(m, name+"_sum", metrics.TypeGauge, m.GetSummary().GetSampleSum())
				appendRow(m, name+"_count", metrics.TypeGauge, float64(m.GetSummary().GetSampleCount()))
			}
		}
	}
	return rows
}

func (s *Scraper) warnDowncast(name, kind string) {
	if _, seen := s.downcastWarned.LoadOrStore(name, struct{}{}); !seen {
		log.Warn().Str("metric", name).Str("kind", kind).
			Msg("downcasting to _sum/_count gauges; buckets/quantiles are dropped")
	}
}

func promLabelsJSON(pairs []*dto.LabelPair) string {
	if len(pairs) == 0 {
		return ""
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p.GetName() != "" {
			m[p.GetName()] = p.GetValue()
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
