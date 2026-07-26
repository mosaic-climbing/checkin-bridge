// Package ui provides the HTMX-based staff dashboard, embedded via go:embed.
package ui

import (
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed templates static
var templateFS embed.FS

// StaticHandler serves the vendored front-end assets (htmx) from the
// embedded filesystem, mounted at /ui/static/. Vendoring exists so the
// UI — including the login page — works with no internet: the gym's
// most valuable dashboard moment is exactly when the WAN is down.
// Assets are filename-versioned (htmx-2.0.4.min.js), so they get an
// aggressive immutable cache header, overriding the security
// middleware's blanket no-store.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(templateFS, "static")
	if err != nil {
		// Embed is compile-time; a missing directory is a build defect,
		// not a runtime condition.
		panic("ui: embedded static directory missing: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// Handler serves the staff UI with HTMX fragments.
type Handler struct {
	layout string
	login  string
	pages  map[string]string
}

// New creates a UI handler by loading all embedded templates.
func New() (*Handler, error) {
	h := &Handler{pages: make(map[string]string)}

	// Load layout
	data, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, err
	}
	h.layout = string(data)

	// Load login
	data, err = templateFS.ReadFile("templates/login.html")
	if err != nil {
		return nil, err
	}
	h.login = string(data)

	// Load page templates
	entries, err := templateFS.ReadDir("templates/pages")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".html")
		data, err := templateFS.ReadFile("templates/pages/" + e.Name())
		if err != nil {
			return nil, err
		}
		h.pages[name] = string(data)
	}

	return h, nil
}

// ServeLogin renders the login page.
func (h *Handler) ServeLogin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, h.login)
}

// HasPage reports whether a page template with the given name exists.
// Routing uses this as the allowlist for path-derived page names.
func (h *Handler) HasPage(page string) bool {
	_, ok := h.pages[page]
	return ok
}

// ServePage renders a page within the layout, or just the page fragment for HTMX requests.
//
// csrfToken, if non-empty, is substituted for the {{CSRF_TOKEN}} placeholder
// in the layout so HTMX requests can echo it back in the X-CSRF-Token header.
// HTMX fragment responses don't re-inject the token — they inherit hx-headers
// from the <body> element set on the initial full-page render.
//
// Unknown page names 404 — the old silent dashboard fallback masked
// broken links AND made every deep link look like it "worked" while
// showing the wrong page.
func (h *Handler) ServePage(w http.ResponseWriter, r *http.Request, page, csrfToken string) {
	content, ok := h.pages[page]
	if !ok {
		http.NotFound(w, r)
		return
	}

	// If this is an HTMX request, return just the fragment
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, content)
		return
	}

	// Full page: wrap in layout, then inject the CSRF token.
	// Escape the token defensively even though it's hex — future changes to
	// the token format shouldn't be able to break HTML parsing.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.Replace(h.layout, "{{CONTENT}}", content, 1)
	html = strings.ReplaceAll(html, "{{CSRF_TOKEN}}", template.HTMLEscapeString(csrfToken))
	io.WriteString(w, html)
}

// RenderFragment renders a raw HTML fragment (for HTMX partial responses).
func RenderFragment(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, html)
}

// HTMLEscape escapes a string for safe HTML output.
func HTMLEscape(s string) string {
	return template.HTMLEscapeString(s)
}
