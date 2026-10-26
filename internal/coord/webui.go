// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"

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
	// The vendored scripts go by a URL that names their content, so a
	// browser may keep them for good.
	b = bytes.ReplaceAll(b, []byte(`"/static/vendor/`), []byte(`"/static/v/`+vendorVersion()+`/vendor/`))
	uiHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// vendorVersion names the content of the vendored scripts: a hash of every
// file under the UI's vendor/.
var vendorVersion = sync.OnceValue(func() string {
	files := web.FS()
	var names []string
	_ = fs.WalkDir(files, "vendor", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, _ := fs.ReadFile(files, n)
		h.Write([]byte(n + "\x00"))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
})

var vendorTypes = map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}

// gzipped holds the vendored files compressed, made on first request.
var gzipped sync.Map // name -> []byte

// serveVendor serves a vendored script from its versioned URL: cached for
// good when the version is the current one, and gzipped when the browser
// takes it (Mermaid is over 5 MB).
func serveVendor(w http.ResponseWriter, r *http.Request, files fs.FS) {
	name := path.Clean(r.PathValue("file"))
	if !strings.HasPrefix(name, "vendor/") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	uiHeaders(w)
	h := w.Header()
	if r.PathValue("ver") == vendorVersion() {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	// Named here, not looked up: Windows takes .js types from its registry.
	if t := vendorTypes[path.Ext(name)]; t != "" {
		h.Set("Content-Type", t)
	} else if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		h.Set("Content-Type", t)
	}
	h.Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		z, ok := gzipped.Load(name)
		if !ok {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(b)
			_ = zw.Close()
			z, _ = gzipped.LoadOrStore(name, buf.Bytes())
		}
		b = z.([]byte)
		h.Set("Content-Encoding", "gzip")
	}
	if r.Method == http.MethodHead {
		return
	}
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
	mux.HandleFunc("GET /static/v/{ver}/{file...}", func(w http.ResponseWriter, r *http.Request) { serveVendor(w, r, files) })
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
