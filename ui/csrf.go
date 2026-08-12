package ui

import (
	"net/http"

	"github.com/rs/zerolog/log"
)

func (ur *Router) csrfErrorHandler(w http.ResponseWriter, r *http.Request) {
	log.Error().
		Str("path", r.URL.Path).
		Str("method", r.Method).
		Msg("CSRF validation failed")

	// For HTMX requests, return appropriate error
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Retarget", "body")
		w.Header().Set("HX-Reswap", "innerHTML")
		http.Error(w, "Security validation failed. Please refresh the page and try again.", http.StatusForbidden)
		return
	}

	// For regular requests, redirect to login page
	http.Redirect(w, r, "/login", http.StatusFound)
}
