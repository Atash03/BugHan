// Package web renders BugHan's server-side HTML (templates + static assets,
// embedded so the binary is self-contained).
package web

import (
	"embed"
	"html/template"
	"net/http"
	"strings"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Static serves embedded static assets.
func Static() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
	})
}

type PageData struct {
	Title    string
	Subtitle string
	Error    string
	Notice   string
	Data     map[string]any
}

var tmpl = template.Must(template.New("bughan").Funcs(template.FuncMap{
	// pct renders v/max as a 0–100 int for inline bar heights.
		"pct": func(v, max int64) int {
			if max <= 0 {
				return 0
			}
			p := int(v * 100 / max)
			if v > 0 && p < 3 {
				p = 3
			}
			return p
		},
		// join renders string slices (alert scopes, email lists).
		"join": strings.Join,
}).ParseFS(templatesFS, "templates/*.html"))

// Render writes a page.
func Render(w http.ResponseWriter, status int, page string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, page, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}
