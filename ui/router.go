package ui

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui/templates"
	"github.com/n0rdy/pooml/utils"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

const (
	sessionCookieName   = "PoomlSession"
	sessionCookieMaxAge = 7 * 24 * 60 * 60 // seconds; matches the sessions service expiry
)

type Router struct {
	sessionsService   *services.SessionsService
	throttlingService *services.ThrottlingService
	apiKeysService    *services.ApiKeysService
	pools             *db.Pools
	broadcaster       *ingestion.Broadcaster
	// streamCtx closes open SSE connections; main cancels it via the server's
	// RegisterOnShutdown so stage 1 isn't held hostage by live tails. appCtx
	// (stage 4) would fire too late: Server.Shutdown waits on handlers first.
	streamCtx         context.Context
	authSecret        string
	env               string
	trustProxyHeaders bool
}

func NewRouter(
	sessionsService *services.SessionsService,
	throttlingService *services.ThrottlingService,
	apiKeysService *services.ApiKeysService,
	pools *db.Pools,
	broadcaster *ingestion.Broadcaster,
	streamCtx context.Context,
	authSecret string,
	env string,
	trustProxyHeaders bool,
) *Router {
	return &Router{
		sessionsService:   sessionsService,
		throttlingService: throttlingService,
		apiKeysService:    apiKeysService,
		pools:             pools,
		broadcaster:       broadcaster,
		streamCtx:         streamCtx,
		authSecret:        authSecret,
		env:               env,
		trustProxyHeaders: trustProxyHeaders,
	}
}

func (ur *Router) NewRouter() *chi.Mux {
	router := chi.NewRouter()

	router.Use(securityHeaders(ur.env))
	router.Use(csrfPrevention(ur.csrfErrorHandler, ur.env))

	// unprotected login routes:
	router.Get("/login", ur.loginPage)
	router.With(loginThrottle(ur.throttlingService, ur.trustProxyHeaders)).Post("/login", ur.processLogin)

	// protected routes:
	router.With(sessionAuth(ur.sessionsService)).
		Get("/", ur.homePage)

	router.With(sessionAuth(ur.sessionsService)).
		Post("/logout", ur.processLogout)

	router.Route("/home", func(r chi.Router) {
		r.Use(sessionAuth(ur.sessionsService)) // session auth for all home routes

		r.Get("/volume", ur.homePageVolumeSegment)
		r.Get("/errors", ur.homePageErrorsSegment)
		r.Get("/services", ur.homePageServicesSegment)
	})

	router.Route("/logs", func(r chi.Router) {
		r.Use(sessionAuth(ur.sessionsService)) // session auth for all logs routes

		r.Get("/", ur.logsPage)
		r.Get("/stream", ur.streamLogs)
		r.Get("/export", ur.exportLogs)
		r.Get("/{id:[0-9]+}", ur.logDetailsPage)
	})

	router.Route("/alerts", func(r chi.Router) {
		r.Use(sessionAuth(ur.sessionsService)) // session auth for all alerts routes

		r.Get("/", ur.alertsPage)
		r.Post("/", ur.createAlert)
		r.Get("/{id}/edit", ur.editAlertPage)
		r.Put("/{id}", ur.updateAlert)
		r.Delete("/{id}", ur.deleteAlert)
		r.Post("/{id}/test", ur.dryRunAlert)
	})

	router.Route("/settings", func(r chi.Router) {
		r.Use(sessionAuth(ur.sessionsService)) // session auth for all settings routes

		r.Get("/", ur.settingsPage)
		r.Put("/retention", ur.updateRetentionSettings)
		r.Put("/backup", ur.updateBackupSettings)
		r.Post("/api-keys", ur.createApiKey)
		r.Delete("/api-keys/{id}", ur.deleteApiKey)
	})

	return router
}

// render writes a templ component with the given status. Render errors after
// the header is sent can only be logged.
func (ur *Router) render(w http.ResponseWriter, req *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(req.Context(), w); err != nil {
		log.Error().Err(err).Str("path", req.URL.Path).Msg("template render failed")
	}
}

func (ur *Router) isAuthed(req *http.Request) bool {
	cookie, err := req.Cookie(sessionCookieName)
	return err == nil && ur.sessionsService.IsSessionValid(cookie.Value)
}

func (ur *Router) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   ur.env == common.ProEnv,
		SameSite: http.SameSiteLaxMode,
	})
}

// Auth

func (ur *Router) loginPage(w http.ResponseWriter, req *http.Request) {
	if ur.isAuthed(req) {
		http.Redirect(w, req, "/", http.StatusSeeOther)
		return
	}
	kind, message := "", ""
	if req.URL.Query().Get("err") == "stale" {
		kind, message = "info", "That page went stale while you were away. Log in and you're right back."
	}
	ur.render(w, req, http.StatusOK, templates.LoginPage(kind, message, nosurf.Token(req)))
}

func (ur *Router) processLogin(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		ur.render(w, req, http.StatusBadRequest,
			templates.LoginPage("error", "That didn't come through right. Try again.", nosurf.Token(req)))
		return
	}
	secret := req.PostFormValue("secret")
	if subtle.ConstantTimeCompare([]byte(secret), []byte(ur.authSecret)) != 1 {
		ur.throttlingService.RecordFailure(utils.ClientIP(req, ur.trustProxyHeaders))
		ur.render(w, req, http.StatusUnauthorized,
			templates.LoginPage("error", "That's not it. Deep breath, try again.", nosurf.Token(req)))
		return
	}

	sessionID, _ := ur.sessionsService.CreateSession()
	ur.setSessionCookie(w, sessionID, sessionCookieMaxAge)
	http.Redirect(w, req, "/", http.StatusSeeOther)
}

func (ur *Router) processLogout(w http.ResponseWriter, req *http.Request) {
	if cookie, err := req.Cookie(sessionCookieName); err == nil {
		ur.sessionsService.InvalidateSession(cookie.Value)
	}
	ur.setSessionCookie(w, "", -1)
	http.Redirect(w, req, "/login", http.StatusSeeOther)
}

// Home

func (ur *Router) homePage(w http.ResponseWriter, req *http.Request) {
	ur.render(w, req, http.StatusOK, templates.HomePage(nosurf.Token(req)))
}

// ----------------------------------------------------------------------------
// Handler stubs - implemented in their milestones (settings M4, logs M5,
// home fragments M7, alerts M8). All return 501 until wired.
// ----------------------------------------------------------------------------

// Home fragments live in ui/home.go; logs handlers in ui/logs.go and
// ui/stream.go.

// Alerts
func (ur *Router) alertsPage(w http.ResponseWriter, req *http.Request)    { notImplemented(w) }
func (ur *Router) createAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) editAlertPage(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) updateAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) deleteAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) dryRunAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }

// Settings
func (ur *Router) updateRetentionSettings(w http.ResponseWriter, req *http.Request) {
	notImplemented(w)
}
func (ur *Router) updateBackupSettings(w http.ResponseWriter, req *http.Request) {
	notImplemented(w)
}

func (ur *Router) settingsPage(w http.ResponseWriter, req *http.Request) {
	keys, err := ur.apiKeysService.List(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("list api keys")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	ur.render(w, req, http.StatusOK, templates.SettingsPage(keys, "", "", nosurf.Token(req)))
}

func (ur *Router) createApiKey(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Redirect(w, req, "/settings", http.StatusSeeOther)
		return
	}
	newKey, createErr := ur.apiKeysService.Create(req.Context(), req.PostFormValue("label"))

	keys, err := ur.apiKeysService.List(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("list api keys")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	errMsg, status := "", http.StatusOK
	if createErr != nil {
		errMsg, status = createErr.Error(), http.StatusBadRequest
	}
	ur.render(w, req, status, templates.SettingsPage(keys, newKey, errMsg, nosurf.Token(req)))
}

func (ur *Router) deleteApiKey(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := ur.apiKeysService.Revoke(req.Context(), id); err != nil {
		log.Error().Err(err).Int64("id", id).Msg("revoke api key")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	keys, err := ur.apiKeysService.List(req.Context())
	if err != nil {
		log.Error().Err(err).Msg("list api keys")
		http.Error(w, "something went sideways; try again", http.StatusInternalServerError)
		return
	}
	// HTMX swaps just the section
	ur.render(w, req, http.StatusOK, templates.ApiKeysSection(keys, "", "", nosurf.Token(req)))
}

func notImplemented(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotImplemented)
}
