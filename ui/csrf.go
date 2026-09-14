package ui

import (
	"net/http"

	"github.com/justinas/nosurf"
	"github.com/rs/zerolog/log"
)

func (ur *Router) csrfErrorHandler(w http.ResponseWriter, r *http.Request) {
	log.Error().
		Str("path", r.URL.Path).
		Str("method", r.Method).
		AnErr("reason", nosurf.Reason(r)).
		Msg("CSRF validation failed")

	// Redirect rather than render in place: a fresh GET of /login issues a
	// clean token/cookie pair (err=stale picks the friendly message). HTMX
	// callers need HX-Redirect - fetch follows a real 3xx transparently.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login?err=stale")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/login?err=stale", http.StatusSeeOther)
}
