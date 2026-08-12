package api

import (
	"encoding/json"
	"net/http"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/utils"

	"github.com/rs/zerolog/log"
)

var (
	unauthorizedRespBody    []byte
	tooManyRequestsRespBody []byte
)

func init() {
	var err error
	unauthorizedRespBody, err = json.Marshal(common.ErrorResponse{Code: common.ErrCodeUnauthorized})
	if err != nil {
		panic(err)
	}
	tooManyRequestsRespBody, err = json.Marshal(common.ErrorResponse{Code: common.ErrCodeTooManyRequests})
	if err != nil {
		panic(err)
	}
}

func apiKeyTokenAuth(authSecret string, throttlingService *services.ThrottlingService, trustProxyHeaders bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ip := utils.ClientIP(req, trustProxyHeaders)
			if throttlingService.IsLocked(ip) {
				sendTooManyRequestsResponse(w)
				return
			}

			authHeader := req.Header.Get("X-API-Key")
			if authHeader != authSecret {
				throttlingService.RecordFailure(ip)
				log.Error().Msg("Invalid API key")
				sendUnauthorizedErrorResponse(w)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// securityHeaders middleware sets HTTP security headers on every API response.
// API responses aren't browser-rendered, so CSP is omitted; the rest are
// cheap defense-in-depth in case a response ever ends up loaded by a browser
// (e.g. error pages opened directly).
func securityHeaders(env string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			if env == common.ProEnv {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, req)
		})
	}
}

func sendUnauthorizedErrorResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write(unauthorizedRespBody)
}

func sendTooManyRequestsResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write(tooManyRequestsRespBody)
}
