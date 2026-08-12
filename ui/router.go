package ui

import (
	"net/http"

	"github.com/n0rdy/pooml/services"

	"github.com/go-chi/chi/v5"
)

type Router struct {
	sessionsService   *services.SessionsService
	throttlingService *services.ThrottlingService
	authSecret        string
	env               string
	trustProxyHeaders bool
}

func NewRouter(
	sessionsService *services.SessionsService,
	throttlingService *services.ThrottlingService,
	authSecret string,
	env string,
	trustProxyHeaders bool,
) *Router {
	return &Router{
		sessionsService:   sessionsService,
		throttlingService: throttlingService,
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
		r.Get("/{id}", ur.logDetailsPage)
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

// ----------------------------------------------------------------------------
// Handler stubs - TODO: implement.
// All routes return 501 Not Implemented until their handlers are wired up.
// Kept inline (Forq-style) for now; can be split into ui/handlers_*.go
// files once they grow.
// ----------------------------------------------------------------------------

// Auth
func (ur *Router) loginPage(w http.ResponseWriter, req *http.Request)     { notImplemented(w) }
func (ur *Router) processLogin(w http.ResponseWriter, req *http.Request)  { notImplemented(w) }
func (ur *Router) processLogout(w http.ResponseWriter, req *http.Request) { notImplemented(w) }

// Home
func (ur *Router) homePage(w http.ResponseWriter, req *http.Request)              { notImplemented(w) }
func (ur *Router) homePageVolumeSegment(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) homePageErrorsSegment(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) homePageServicesSegment(w http.ResponseWriter, req *http.Request) {
	notImplemented(w)
}

// Logs
func (ur *Router) logsPage(w http.ResponseWriter, req *http.Request)       { notImplemented(w) }
func (ur *Router) streamLogs(w http.ResponseWriter, req *http.Request)     { notImplemented(w) }
func (ur *Router) logDetailsPage(w http.ResponseWriter, req *http.Request) { notImplemented(w) }

// Alerts
func (ur *Router) alertsPage(w http.ResponseWriter, req *http.Request)    { notImplemented(w) }
func (ur *Router) createAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) editAlertPage(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) updateAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) deleteAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }
func (ur *Router) dryRunAlert(w http.ResponseWriter, req *http.Request)   { notImplemented(w) }

// Settings
func (ur *Router) settingsPage(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) updateRetentionSettings(w http.ResponseWriter, req *http.Request) {
	notImplemented(w)
}
func (ur *Router) updateBackupSettings(w http.ResponseWriter, req *http.Request) {
	notImplemented(w)
}
func (ur *Router) createApiKey(w http.ResponseWriter, req *http.Request) { notImplemented(w) }
func (ur *Router) deleteApiKey(w http.ResponseWriter, req *http.Request) { notImplemented(w) }

func notImplemented(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotImplemented)
}
