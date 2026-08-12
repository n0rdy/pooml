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

	// For HTMX requests, return appropriate error
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Retarget", "body")
		w.Header().Set("HX-Reswap", "innerHTML")
		http.Error(w, "Security validation failed. Please refresh the page and try again.", http.StatusForbidden)
		return
	}

	// Regular requests: redirect rather than render in place. A fresh GET of
	// /login issues a clean token/cookie pair, which is the reliable way out
	// of whatever staleness caused the failure; err=stale picks the friendly
	// message on the login page.
	http.Redirect(w, r, "/login?err=stale", http.StatusSeeOther)
}
