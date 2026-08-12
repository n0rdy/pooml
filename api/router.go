package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/services"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

const healthcheckTimeout = 2 * time.Second

type Router struct {
	monitoringService *services.MonitoringService
	throttlingService *services.ThrottlingService
	authSecret        string
	env               string
	trustProxyHeaders bool
}

func NewRouter(
	monitoringService *services.MonitoringService,
	throttlingService *services.ThrottlingService,
	authSecret string,
	env string,
	trustProxyHeaders bool,
) *Router {
	return &Router{
		monitoringService: monitoringService,
		throttlingService: throttlingService,
		authSecret:        authSecret,
		env:               env,
		trustProxyHeaders: trustProxyHeaders,
	}
}

func (ar *Router) NewRouter() *chi.Mux {
	router := chi.NewRouter()

	router.Use(securityHeaders(ar.env))

	// Operational endpoints - outside /api/v1, unprotected.
	// Healthcheck must be reachable by load balancers / Coolify probes
	// without API-key auth.
	router.Get("/healthcheck", ar.healthcheck)

	router.Route("/api/v1", func(r chi.Router) {
		r.Use(apiKeyTokenAuth(ar.authSecret, ar.throttlingService, ar.trustProxyHeaders))

		r.Post("/ingest/{service}/{host}", ar.ingestLogs)
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

// TODO: implement - log ingestion entry point. Reads body (capped at 2 MB),
// extracts service/host from URL, pushes to ingestChan, returns 204.
func (ar *Router) ingestLogs(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
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
