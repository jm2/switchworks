// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"io/fs"
	"mime"
	"net/http"

	"github.com/jm2/switchworks/internal/web"
)

// The UI embeds its fonts; Go's built-in MIME table does not know WOFF2,
// and the pages are served with nosniff.
func init() { _ = mime.AddExtensionType(".woff2", "font/woff2") }

const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func uiHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
}

func servePage(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	b, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	uiHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// uiRoutes adds the web UI: pages, static files, sign-in and the CA
// certificate (public, so phones can install it).
func (s *Server) uiRoutes(mux *http.ServeMux) {
	files := web.FS()
	static := http.StripPrefix("/static/", http.FileServer(http.FS(files)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiHeaders(w)
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !s.cookieAuthed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		servePage(w, r, files, "index.html")
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { servePage(w, r, files, "login.html") })
	mux.HandleFunc("POST /ui/login", bodyDeadline(s.handleLogin))
	mux.HandleFunc("POST /ui/logout", s.handleLogout)
	mux.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="switchworks-ca.crt"`)
		_, _ = w.Write(s.ca.PEM)
	})
}
