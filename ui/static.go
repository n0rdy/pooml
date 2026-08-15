package ui

import (
	_ "embed"
	"net/http"
)

//go:embed static/logo.png
var logoPNG []byte

func (ur *Router) logo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(logoPNG)
}
