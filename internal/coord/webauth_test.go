// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/ulid"
)

func TestCookies(t *testing.T) {
	a, err := loadWebAuth(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := func(c string, at time.Time) bool { _, ok := a.validCookie(c, at); return ok }
	admin := grant{scope: scopeAdmin}
	c := a.newCookie(now, admin, time.Time{})
	if g, ok := a.validCookie(c, now); !ok || g != admin {
		t.Fatalf("fresh cookie: %+v %v", g, ok)
	}
	if valid(c, now.Add(cookieTTL+time.Minute)) {
		t.Fatal("expired cookie accepted")
	}
	bad := []byte(c)
	bad[3] ^= 1
	if valid(string(bad), now) || valid("x.y", now) || valid("", now) {
		t.Fatal("tampered cookie accepted")
	}
	other, _ := loadWebAuth(t.TempDir())
	if _, ok := other.validCookie(c, now); ok {
		t.Fatal("cookie valid under another key")
	}

	// The grant is signed into the cookie.
	read := grant{scope: scopeRead, tokenID: ulid.New()}
	rc := a.newCookie(now, read, now.Add(time.Hour))
	if g, ok := a.validCookie(rc, now); !ok || g != read {
		t.Fatalf("scoped cookie: %+v %v", g, ok)
	}
	if valid(rc, now.Add(2*time.Hour)) {
		t.Fatal("cookie outlived its token's expiry")
	}
	// Raising the scope byte breaks the signature.
	p, sig, _ := strings.Cut(rc, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(p)
	raw[cookieIDEnd] = byte(scopeAdmin)
	if valid(base64.RawURLEncoding.EncodeToString(raw)+"."+sig, now) {
		t.Fatal("cookie with a forged scope accepted")
	}
	// A pre-token cookie (expiry and id only) is still the admin's.
	legacy := make([]byte, legacyCookieLen)
	binary.BigEndian.PutUint64(legacy, uint64(now.Add(time.Hour).Unix()))
	lc := base64.RawURLEncoding.EncodeToString(legacy) + "." + base64.RawURLEncoding.EncodeToString(a.sign(legacy))
	if g, ok := a.validCookie(lc, now); !ok || g != admin {
		t.Fatalf("legacy cookie: %+v %v", g, ok)
	}

	code := a.newCode(now, time.Minute, read)
	if g, ok := a.useCode(code, now); !ok || g != read {
		t.Fatalf("login code: %+v %v", g, ok)
	}
	if _, ok := a.useCode(code, now); ok {
		t.Fatal("login code must work exactly once")
	}
	if late := a.newCode(now, time.Minute, admin); func() bool { _, ok := a.useCode(late, now.Add(2*time.Minute)); return ok }() {
		t.Fatal("expired login code accepted")
	}
}

type webEnv struct {
	srv   *Server
	http  *httptest.Server
	token string
}

func newWebEnv(t *testing.T) *webEnv {
	t.Helper()
	dir := t.TempDir()
	token, err := Init(dir, []string{"127.0.0.1"}, 7480, 7481)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenFileStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(dir, cfg, st, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		s.Shutdown()
		_ = st.Close()
	})
	return &webEnv{srv: s, http: hs, token: token}
}

func (e *webEnv) do(t *testing.T, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	if strings.HasPrefix(path, "/ui/") {
		req.Header.Set("X-Swyd-CSRF", "1") // as login.js and app.js send it
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestWebSignIn(t *testing.T) {
	e := newWebEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + e.token}

	if r := e.do(t, "GET", "/", "", nil); r.StatusCode != 302 || r.Header.Get("Location") != "/login" {
		t.Fatalf("GET / signed out: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.do(t, "GET", "/login", "", nil); r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("GET /login: %d %v", r.StatusCode, r.Header)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", nil); r.StatusCode != 401 {
		t.Fatalf("API signed out: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Authorization": "Bearer nope"}); r.StatusCode != 401 {
		t.Fatalf("bad bearer: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/ui/login", `{"token":"wrong"}`, nil); r.StatusCode != 401 {
		t.Fatalf("bad token sign-in: %d", r.StatusCode)
	}
	r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil)
	if r.StatusCode != 200 || len(r.Cookies()) != 1 {
		t.Fatalf("sign-in: %d %v", r.StatusCode, r.Cookies())
	}
	ck := r.Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Secure { // plain HTTP test server: not Secure
		t.Fatalf("cookie flags: %+v", ck)
	}
	cookie := map[string]string{"Cookie": ck.Name + "=" + ck.Value}

	if r := e.do(t, "GET", "/", "", cookie); r.StatusCode != 200 {
		t.Fatalf("GET / signed in: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", cookie); r.StatusCode != 200 {
		t.Fatalf("API with cookie: %d", r.StatusCode)
	}
	// Writes with a cookie need the CSRF header.
	msg := `{"to":"user:jm","text":"hi"}`
	if r := e.do(t, "POST", "/v1/messages", msg, cookie); r.StatusCode != 403 {
		t.Fatalf("cookie write without CSRF header: %d", r.StatusCode)
	}
	withCSRF := map[string]string{"Cookie": cookie["Cookie"], "X-Swyd-CSRF": "1"}
	if r := e.do(t, "POST", "/v1/messages", msg, withCSRF); r.StatusCode != 202 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("cookie write with CSRF header: %d %s", r.StatusCode, b)
	}
	if r := e.do(t, "POST", "/v1/messages", msg, bearer); r.StatusCode != 202 {
		t.Fatalf("bearer write: %d", r.StatusCode)
	}

	// One-time links.
	r = e.do(t, "POST", "/v1/ui-links", `{}`, bearer)
	var link struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&link)
	_, code, ok := strings.Cut(link.URL, "/login#code=")
	if r.StatusCode != 200 || !ok || !strings.HasPrefix(link.URL, "https://127.0.0.1:7480/") {
		t.Fatalf("ui-link: %d %q", r.StatusCode, link.URL)
	}
	if r := e.do(t, "POST", "/ui/login", `{"code":"`+code+`"}`, nil); r.StatusCode != 200 {
		t.Fatalf("code sign-in: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/ui/login", `{"code":"`+code+`"}`, nil); r.StatusCode != 401 {
		t.Fatalf("code reused: %d", r.StatusCode)
	}

	// Static files and the CA.
	if r := e.do(t, "GET", "/static/app.js", "", nil); r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("app.js: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := e.do(t, "GET", "/ca.crt", "", nil); r.StatusCode != 200 {
		t.Fatalf("ca.crt: %d", r.StatusCode)
	} else if b, _ := io.ReadAll(r.Body); !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatal("ca.crt is not a certificate")
	}

	// Sign-in failures are throttled per client (one failure above, this
	// makes ten), but a one-time link still works.
	for i := 0; i < 9; i++ {
		e.do(t, "POST", "/ui/login", `{"token":"wrong"}`, nil)
	}
	if r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil); r.StatusCode != 429 {
		t.Fatalf("throttle: %d", r.StatusCode)
	}
	code = e.srv.web.newCode(time.Now(), time.Minute, grant{scope: scopeAdmin})
	if r := e.do(t, "POST", "/ui/login", `{"code":"`+code+`"}`, nil); r.StatusCode != 200 {
		t.Fatalf("link sign-in while tokens are throttled: %d", r.StatusCode)
	}
	if v, th := e.srv.web.checkToken("10.0.0.9", time.Now(), func() bool { return true }); !v || th {
		t.Fatal("another client was throttled too")
	}

	// Signing out revokes that cookie on the server, not just in the browser.
	if r := e.do(t, "POST", "/ui/logout", "", cookie); r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", cookie); r.StatusCode != 401 {
		t.Fatalf("API with a signed-out cookie: %d", r.StatusCode)
	}
	if again, _ := loadWebAuth(filepath.Dir(e.srv.web.revokePath)); func() bool { _, ok := again.validCookie(ck.Value, time.Now()); return ok }() {
		t.Fatal("revocation not persisted")
	}
	// Without the CSRF header, neither sign-in nor sign-out is accepted.
	req, _ := http.NewRequest("POST", e.http.URL+"/ui/login", strings.NewReader(`{"token":"x"}`))
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 403 {
		t.Fatalf("sign-in without CSRF header: %v %v", r, err)
	} else {
		r.Body.Close()
	}
}

func TestFleetStream(t *testing.T) {
	e := newWebEnv(t)
	req, _ := http.NewRequest("GET", e.http.URL+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var typ string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{typ, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()
	next := func() [2]string {
		select {
		case ev := <-events:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		return [2]string{}
	}
	if ev := next(); ev[0] != "hello" || !strings.Contains(ev[1], `"sessions":[]`) {
		t.Fatalf("first event: %v", ev)
	}
	e.do(t, "POST", "/v1/messages", `{"to":"user:jm","text":"ping","from":"user:ops"}`, map[string]string{"Authorization": "Bearer " + e.token})
	ev := next()
	if ev[0] != "message" || !strings.Contains(ev[1], `"text":"ping"`) || !strings.Contains(ev[1], `"from":"user:ops"`) {
		t.Fatalf("message event: %v", ev)
	}
}
