package server

import (
	_ "embed"
	"html"
	"net/http"
	"strings"
)

// Version is set by release builds with -ldflags -X.
var Version = "v26.11.02.1"

//go:embed panel.html
var panelHTMLRaw string

//go:embed panel.js
var panelJS string

//go:embed panel.css
var panelCSS string

func panelHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/admin" && r.URL.Path != "/panel" && r.URL.Path != "/panel/" {
		http.NotFound(w, r)
		return
	}
	panelHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.ReplaceAll(panelHTMLRaw, "{{VERSION}}", html.EscapeString(Version))))
}

func (h *Handler) servePanelAsset(w http.ResponseWriter, r *http.Request) {
	var body, contentType string
	switch r.URL.Path {
	case "/panel/assets/app.js":
		body, contentType = panelJS, "text/javascript; charset=utf-8"
	case "/panel/assets/app.css":
		body, contentType = panelCSS, "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	panelHeaders(w)
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write([]byte(body))
}
