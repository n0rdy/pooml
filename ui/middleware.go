package ui

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"
	"github.com/n0rdy/pooml/utils"

	"github.com/a-h/templ"
	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

func newNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure means the process is unusable anyway
	}
	return base64.StdEncoding.EncodeToString(b)
}

// securityHeaders middleware sets HTTP security headers on every UI response.
// script-src is nonce-based (no 'unsafe-inline'): every inline <script> in
// the templates carries nonce={ templ.GetNonce(ctx) }, so an injected script
// from a hypothetical escaping gap never executes. htmx needs no inline
// allowance (we use none of its eval-dependent features). style-src keeps
// 'unsafe-inline' for the template <style> blocks - style injection is not
// the threat CSP is load-bearing for here.
func securityHeaders(env string) func(http.Handler) http.Handler {
	cspTemplate := strings.Join([]string{
		"default-src 'self'",
		// no CDN hosts anywhere: every asset is vendored + embedded, so a
		// page that tries to reach one is a bug (or an injection)
		"script-src 'self' 'nonce-%s'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"frame-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			nonce := newNonce()
			req = req.WithContext(templ.WithNonce(req.Context(), nonce))
			h := w.Header()
			h.Set("Content-Security-Policy", fmt.Sprintf(cspTemplate, nonce))
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			if env == common.ProEnv {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, req)
		})
	}
}

// loginThrottle blocks login attempts from IPs past the failure threshold.
// Renders the full login page at 429 rather than a bare error, so a locked-out
// human still sees a recognizable screen.
func loginThrottle(throttlingService *services.ThrottlingService, trustProxyHeaders bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ip := utils.ClientIP(req, trustProxyHeaders)
			if throttlingService.IsLocked(ip) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				page := templates.LoginPage("throttled",
					"Too many attempts. The door is locked for a minute - catch your breath.",
					nosurf.Token(req))
				if err := page.Render(req.Context(), w); err != nil {
					log.Error().Err(err).Msg("throttle page render failed")
				}
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// sessionAuth middleware for UI routes
func sessionAuth(sessionsService *services.SessionsService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			sessionCookie, err := req.Cookie("PoomlSession")
			if err != nil {
				http.Redirect(w, req, "/login", http.StatusFound)
				return
			}

			sessionId := sessionCookie.Value
			if !sessionsService.IsSessionValid(sessionId) {
				http.Redirect(w, req, "/login", http.StatusFound)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// csrfPrevention middleware to protect against CSRF attacks.
// Note: nosurf v1.2 additionally requires every unsafe request to carry one of
// Sec-Fetch-Site/Origin/Referer and match the host. Browsers always send one
// on form posts and HTMX requests; bare HTTP clients (tests, curl) must set
// Origin explicitly or they fail with ErrNoReferer.
func csrfPrevention(csrfFailureHandler func(w http.ResponseWriter, r *http.Request), env string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		csrfHandler := nosurf.New(next)
		csrfHandler.SetBaseCookie(http.Cookie{
			HttpOnly: true,
			Path:     "/",
			Secure:   env == common.ProEnv,
			SameSite: http.SameSiteLaxMode,
		})

		csrfHandler.SetFailureHandler(http.HandlerFunc(csrfFailureHandler))

		// we are using HTTP in local env, so we need to disable the Secure flag check
		if env == common.LocalEnv {
			csrfHandler.SetIsTLSFunc(func(r *http.Request) bool {
				return false
			})
		}
		return csrfHandler
	}
}
