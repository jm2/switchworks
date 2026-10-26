// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/web"
)

// The page names the vendored scripts by their content's version; at that
// version they are cached for good and gzipped for a browser that takes
// it, at any other they are served but not kept.
func TestVendorScripts(t *testing.T) {
	files := web.FS()
	rec := httptest.NewRecorder()
	servePage(rec, httptest.NewRequest("GET", "/", nil), files, "index.html")
	ver := vendorVersion()
	if !strings.Contains(rec.Body.String(), `"/static/v/`+ver+`/vendor/markdown-it.min.js"`) || strings.Contains(rec.Body.String(), `"/static/vendor/`) {
		t.Fatalf("index.html does not name the versioned scripts:\n%s", rec.Body.String())
	}
	want, err := fs.ReadFile(files, "vendor/markdown-it.min.js")
	if err != nil {
		t.Fatal(err)
	}

	e := newWebEnv(t)
	r := e.do(t, "GET", "/static/v/"+ver+"/vendor/markdown-it.min.js", "", map[string]string{"Accept-Encoding": "gzip"})
	if r.StatusCode != 200 || r.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("versioned script: %d %v", r.StatusCode, r.Header)
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, want) {
		t.Fatalf("gzipped script differs: %d bytes, want %d", len(got), len(want))
	}

	r = e.do(t, "GET", "/static/v/0000/vendor/markdown-it.min.js", "", map[string]string{"Accept-Encoding": "identity"})
	got, _ = io.ReadAll(r.Body)
	if r.StatusCode != 200 || r.Header.Get("Content-Encoding") != "" || strings.Contains(r.Header.Get("Cache-Control"), "immutable") || !bytes.Equal(got, want) {
		t.Fatalf("another version: %d %v", r.StatusCode, r.Header)
	}
	for _, p := range []string{"/static/v/" + ver + "/app.js", "/static/v/" + ver + "/index.html", "/static/v/" + ver + "/vendor/nope.js"} {
		if r := e.do(t, "GET", p, "", nil); r.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, r.StatusCode)
		}
	}
}
