package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/metrics"
	"github.com/n0rdy/pooml/services"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

const (
	healthcheckTimeout = 2 * time.Second

	// matches FluentBit's default chunk size; see CONTEXT.md > Ingestion
	maxIngestPayloadBytes = 2 << 20

	maxOTLPRowsPerRequest = 50000
)

type Router struct {
	monitoringService *services.MonitoringService
	throttlingService *services.ThrottlingService
	apiKeysService    *services.ApiKeysService
	pipeline          *ingestion.Pipeline
	metricsPipeline   *metrics.Pipeline
	env               string
	trustProxyHeaders bool
	metricsSecret     string

	downcastWarned sync.Map // metric name -> struct{}; once-per-name OTLP downcast warning
}

func NewRouter(
	monitoringService *services.MonitoringService,
	throttlingService *services.ThrottlingService,
	apiKeysService *services.ApiKeysService,
	pipeline *ingestion.Pipeline,
	metricsPipeline *metrics.Pipeline,
	env string,
	trustProxyHeaders bool,
	metricsSecret string, // empty = /metrics disabled
) *Router {
	return &Router{
		monitoringService: monitoringService,
		throttlingService: throttlingService,
		apiKeysService:    apiKeysService,
		pipeline:          pipeline,
		metricsPipeline:   metricsPipeline,
		env:               env,
		trustProxyHeaders: trustProxyHeaders,
		metricsSecret:     metricsSecret,
	}
}

func (ar *Router) NewRouter() *chi.Mux {
	router := chi.NewRouter()

	router.Use(securityHeaders(ar.env))

	// Operational endpoints - outside /api/v1, unprotected.
	// Healthcheck must be reachable by load balancers / Coolify probes
	// without API-key auth.
	router.Get("/healthcheck", ar.healthcheck)

	if ar.metricsSecret != "" {
		router.With(metricsSecretAuth(ar.metricsSecret, ar.throttlingService, ar.trustProxyHeaders)).
			Method(http.MethodGet, "/metrics",
				promhttp.HandlerFor(services.PromRegistry, promhttp.HandlerOpts{}))
	}

	router.Route("/api/v1", func(r chi.Router) {
		r.Use(apiKeyTokenAuth(ar.apiKeysService, ar.throttlingService, ar.trustProxyHeaders))

		r.Post("/ingest/{service}/{host}", ar.ingestLogs)
		r.Post("/otlp/v1/metrics", ar.otlpMetrics)
	})

	return router
}

func (ar *Router) healthcheck(w http.ResponseWriter, req *http.Request) {
	// Bound the ping budget so a stuck DB can't hold the probe open and
	// trigger probe timeouts at the orchestrator layer (Coolify, k8s, etc.).
	ctx, cancel := context.WithTimeout(req.Context(), healthcheckTimeout)
	defer cancel()

	if ar.monitoringService.IsHealthy(ctx) {
		ar.sendNoContentEmptyResponse(w)
		return
	}
	ar.sendErrorResponse(w, http.StatusServiceUnavailable, common.ErrCodeServiceUnhealthy)
}

// ingestLogs runs on the request goroutine and must stay fast: no parsing
// here, just a bounded read and a non-blocking push onto the pipeline.
func (ar *Router) ingestLogs(w http.ResponseWriter, req *http.Request) {
	service := chi.URLParam(req, "service")
	host := chi.URLParam(req, "host")
	if len(service) > 200 || len(host) > 200 {
		ar.sendErrorResponse(w, http.StatusBadRequest, common.ErrCodeBadRequest)
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxIngestPayloadBytes)
	payload, err := io.ReadAll(req.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			ar.sendErrorResponse(w, http.StatusRequestEntityTooLarge, common.ErrCodePayloadTooLarge)
			return
		}
		ar.sendErrorResponse(w, http.StatusBadRequest, common.ErrCodeBadRequest)
		return
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		ar.sendErrorResponse(w, http.StatusBadRequest, common.ErrCodeEmptyPayload)
		return
	}

	if !ar.pipeline.TryIngest(service, host, payload, time.Now().UnixMilli()) {
		// pipeline saturated: FluentBit retries with backoff
		ar.sendErrorResponse(w, http.StatusTooManyRequests, common.ErrCodeTooManyRequests)
		return
	}
	ar.sendNoContentEmptyResponse(w)
}

func (ar *Router) sendNoContentEmptyResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func (ar *Router) sendJsonResponse(w http.ResponseWriter, httpCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Error().Err(err).Msg("marshal response body")
		// Fallback: best-effort 500 with no body - we already failed to marshal once.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	w.Write(body)
}

func (ar *Router) sendErrorResponse(w http.ResponseWriter, httpCode int, errCode string) {
	ar.sendJsonResponse(w, httpCode, common.ErrorResponse{Code: errCode})
}
