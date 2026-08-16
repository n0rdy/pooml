package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

// All frontend assets ship inside the binary: styles.css (built by
// ui/assets, committed) and vendor/ (tools/vendor-assets.sh, committed).
// No CDN is ever contacted - pooml works fully offline.
//
//go:embed static
var staticFS embed.FS

//go:embed static/logo.png
var logoPNG []byte

func (ur *Router) logo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(logoPNG)
}

func (ur *Router) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed guarantees the directory exists
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, req)
	})
}
