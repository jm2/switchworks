// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
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
	// A pre-token cookie (expiry and id only) was the admin's, and carries
	// no admin generation: refused.
	legacy := make([]byte, legacyCookieLen)
	binary.BigEndian.PutUint64(legacy, uint64(now.Add(time.Hour).Unix()))
	lc := base64.RawURLEncoding.EncodeToString(legacy) + "." + base64.RawURLEncoding.EncodeToString(a.sign(legacy))
	if valid(lc, now) {
		t.Fatal("admin cookie without a generation accepted")
	}
	// So is an admin cookie from before generations: the issued flag only.
	pre := make([]byte, cookieIDEnd, cookieIDEnd+9)
	binary.BigEndian.PutUint64(pre, uint64(now.Add(time.Hour).Unix()))
	pre = append(pre, byte(scopeAdmin)|cookieIssuedFlag)
	pre = binary.BigEndian.AppendUint64(pre, uint64(now.UnixNano()))
	if valid(base64.RawURLEncoding.EncodeToString(pre)+"."+base64.RawURLEncoding.EncodeToString(a.sign(pre)), now) {
		t.Fatal("admin cookie from before generations accepted")
	}
	// Replacing the admin token signs its browsers out, and only them.
	a.adminGen = adminGeneration("new hash")
	if valid(c, now) {
		t.Fatal("admin cookie survived a new admin token")
	}
	if !valid(rc, now) {
		t.Fatal("a new admin token signed out an API token's cookie")
	}
	if !valid(a.newCookie(now, admin, time.Time{}), now) {
		t.Fatal("admin cookie under the new token refused")
	}

	code := a.newCode(now, time.Minute, read)
	if g, ok, _ := a.useCode(code, now); !ok || g != read {
		t.Fatalf("login code: %+v %v", g, ok)
	}
	if _, ok, _ := a.useCode(code, now); ok {
		t.Fatal("login code must work exactly once")
	}
	if late := a.newCode(now, time.Minute, admin); func() bool { _, ok, _ := a.useCode(late, now.Add(2*time.Minute)); return ok }() {
		t.Fatal("expired login code accepted")
	}

	// A link refused for too many sign-ins with its credential is not used
	// up.
	busy := grant{scope: scopeRead, tokenID: ulid.New()}
	for i := 0; i < maxSignInsPerMinute; i++ {
		a.allowSignIn(busy, now)
	}
	code = a.newCode(now, loginLinkTT, busy)
	if _, ok, th := a.useCode(code, now); ok || !th {
		t.Fatalf("link past the sign-in limit: ok %v throttled %v", ok, th)
	}
	if g, ok, th := a.useCode(code, now.Add(time.Minute)); !ok || th || g != busy {
		t.Fatalf("link after the limit passed: %+v ok %v throttled %v", g, ok, th)
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
		req.Header.Set("X-Swk-CSRF", "1") // as login.js and app.js send it
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
	withCSRF := map[string]string{"Cookie": cookie["Cookie"], "X-Swk-CSRF": "1"}
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
	// makes ten), but the right token and a one-time link still work.
	for i := 0; i < 9; i++ {
		e.do(t, "POST", "/ui/login", `{"token":"wrong"}`, nil)
	}
	if r := e.do(t, "POST", "/ui/login", `{"token":"wrong"}`, nil); r.StatusCode != 429 {
		t.Fatalf("throttle: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil); r.StatusCode != 200 {
		t.Fatalf("valid token sign-in while throttled: %d", r.StatusCode)
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

func TestSignOutBounded(t *testing.T) {
	dir := t.TempDir()
	a, err := loadWebAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	admin, tok := grant{scope: scopeAdmin}, grant{scope: scopeRead, tokenID: ulid.New()}
	valid := func(c string, at time.Time) bool { _, ok := a.validCookie(c, at); return ok }
	early := a.newCookie(now, admin, time.Time{}) // another browser, signed in first
	other := a.newCookie(now, tok, time.Time{})   // another credential's browser
	var first string
	for i := 0; i < maxRevokedPerCred+50; i++ {
		c := a.newCookie(now.Add(time.Duration(i+1)*time.Millisecond), admin, time.Time{})
		if i == 0 {
			first = c
		}
		if err := a.revoke(c, now); err != nil {
			t.Fatal(err)
		}
	}
	late := a.newCookie(now.Add(time.Second), admin, time.Time{})
	if n := len(a.revoked.Cookies); n > maxRevokedPerCred {
		t.Fatalf("%d sign-outs kept, want at most %d", n, maxRevokedPerCred)
	}
	// Folding signs out what came before the folded cookies, and nothing
	// newer or of another credential.
	if valid(first, now) || valid(early, now) {
		t.Fatal("folded sign-out let a cookie back in")
	}
	if !valid(late, now) || !valid(other, now) {
		t.Fatal("folding signed out a newer browser or another credential")
	}
	again, err := loadWebAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.validCookie(first, now); ok {
		t.Fatal("cutoff not persisted")
	}
	if _, ok := again.validCookie(late, now); !ok {
		t.Fatal("cookie after the cutoff refused after a reload")
	}
}

func TestSignOutOldFormat(t *testing.T) {
	dir := t.TempDir()
	a, _ := loadWebAuth(dir)
	now := time.Now()
	c := a.newCookie(now, grant{scope: scopeAdmin}, time.Time{})
	parsed, _ := a.parseCookie(c)
	old, _ := json.Marshal(map[string]time.Time{parsed.id: time.Unix(parsed.expires, 0)})
	if err := os.WriteFile(a.revokePath, old, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := loadWebAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.validCookie(c, now); ok {
		t.Fatal("sign-out from the old file format not honored")
	}
	// A corrupt file is not ignored: the coordinator still starts, with a
	// new key that signs every browser out, signed-out ones included.
	other := b.newCookie(now, grant{scope: scopeAdmin}, time.Time{})
	if err := os.WriteFile(a.revokePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := loadWebAuth(dir)
	if err != nil {
		t.Fatalf("corrupt sign-out file stopped startup: %v", err)
	}
	if r.notice == "" {
		t.Fatal("repair not reported")
	}
	for _, ck := range []string{c, other} {
		if _, ok := r.validCookie(ck, now); ok {
			t.Fatal("a cookie from before the repair still works")
		}
	}
	if aside, _ := filepath.Glob(a.revokePath + ".corrupt-*"); len(aside) != 1 {
		t.Fatalf("corrupt file not kept aside: %v", aside)
	}
	fresh := r.newCookie(now, grant{scope: scopeAdmin}, time.Time{})
	if _, ok := r.validCookie(fresh, now); !ok {
		t.Fatal("new sign-ins refused after the repair")
	}
}

func TestSignOutNotSaved(t *testing.T) {
	e := newWebEnv(t)
	r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil)
	ck := r.Cookies()[0]
	cookie := map[string]string{"Cookie": ck.Name + "=" + ck.Value}
	// A directory where the file goes makes the write fail.
	if err := os.Mkdir(e.srv.web.revokePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := e.do(t, "POST", "/ui/logout", "", cookie); r.StatusCode != 500 {
		t.Fatalf("unsaved sign-out reported as %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", cookie); r.StatusCode != 401 {
		t.Fatalf("cookie still works after an unsaved sign-out: %d", r.StatusCode)
	}
}

func TestBearerThrottle(t *testing.T) {
	e := newWebEnv(t)
	for i := 0; i < maxFailures; i++ {
		if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Authorization": "Bearer nope"}); r.StatusCode != 401 {
			t.Fatalf("bad bearer %d: %d", i, r.StatusCode)
		}
	}
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Authorization": "Bearer nope"}); r.StatusCode != 429 {
		t.Fatalf("bearer guesses not throttled: %d", r.StatusCode)
	}
	// Sign-ins share the count.
	if r := e.do(t, "POST", "/ui/login", `{"token":"nope"}`, nil); r.StatusCode != 429 {
		t.Fatalf("sign-in after bearer guesses: %d", r.StatusCode)
	}
	// Someone else's bad tokens from the same address do not lock out a
	// valid one.
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Authorization": "Bearer " + e.token}); r.StatusCode != 200 {
		t.Fatalf("valid bearer from a throttled client: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil); r.StatusCode != 200 {
		t.Fatalf("valid sign-in from a throttled client: %d", r.StatusCode)
	}
}

func TestAdminCookieRotation(t *testing.T) {
	e := newWebEnv(t)
	r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil)
	ck := r.Cookies()[0]
	cookie := map[string]string{"Cookie": ck.Name + "=" + ck.Value}
	if r := e.do(t, "GET", "/v1/hosts", "", cookie); r.StatusCode != 200 {
		t.Fatalf("admin cookie: %d", r.StatusCode)
	}
	// A restart with a new admin_token_hash, and the same ui.key.
	cfg := e.srv.cfg
	cfg.AdminTokenHash = pki.HashSecret(pki.RandomSecret(32))
	s, err := NewServer(e.srv.dir, cfg, e.srv.store, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/hosts", nil)
	req.Header.Set("Cookie", cookie["Cookie"])
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("admin cookie after the admin token was replaced: %d", rec.Code)
	}
}

func TestFailuresEvictOldest(t *testing.T) {
	a, _ := loadWebAuth(t.TempDir())
	now := time.Now()
	bad := func() bool { return false }
	for i := 0; i < maxFailures; i++ {
		a.checkToken("attacker", now, bad)
	}
	// A flood of other clients, all failing more recently than the attacker
	// but within the minute.
	for i := 0; i < 2*maxClients; i++ {
		a.checkToken(fmt.Sprintf("10.%d.%d.0/24", i>>8, i&0xff), now.Add(time.Second), bad)
	}
	if len(a.failures) > maxClients {
		t.Fatalf("%d clients tracked", len(a.failures))
	}
	// The quietest clients are forgotten first: one that failed last keeps
	// its count through another flood.
	for i := 0; i < maxFailures; i++ {
		a.checkToken("busy", now.Add(2*time.Second), bad)
	}
	for i := 0; i < maxClients; i++ {
		a.checkToken(fmt.Sprintf("11.%d.%d.0/24", i>>8, i&0xff), now.Add(time.Second), bad)
	}
	if _, th := a.checkToken("busy", now.Add(3*time.Second), bad); !th {
		t.Fatal("a flood of new clients reset an active client's count")
	}
}

func TestHostCookieOverHTTPS(t *testing.T) {
	e := newWebEnv(t)
	// The test server is plain HTTP on loopback, as behind tailscale serve.
	https := map[string]string{"X-Forwarded-Proto": "https"}
	r := e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, https)
	if r.StatusCode != 200 || len(r.Cookies()) != 1 {
		t.Fatalf("sign-in: %d %v", r.StatusCode, r.Cookies())
	}
	ck := r.Cookies()[0]
	if ck.Name != secureCookieName || !ck.Secure || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("cookie over HTTPS: %+v", ck)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Cookie": ck.Name + "=" + ck.Value}); r.StatusCode != 200 {
		t.Fatalf("API with the __Host- cookie: %d", r.StatusCode)
	}
	// A swk_session cookie, which a sibling host could have planted, is
	// refused over HTTPS (and still works over plain HTTP).
	plain := e.srv.web.newCookie(time.Now(), grant{scope: scopeAdmin}, time.Time{})
	planted := map[string]string{"Cookie": cookieName + "=" + plain, "X-Forwarded-Proto": "https"}
	if r := e.do(t, "GET", "/v1/hosts", "", planted); r.StatusCode != 401 {
		t.Fatalf("API over HTTPS with swk_session: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Cookie": cookieName + "=" + plain}); r.StatusCode != 200 {
		t.Fatalf("API over plain HTTP with swk_session: %d", r.StatusCode)
	}
	// Signing out over HTTPS leaves a planted cookie alone (it was never
	// the browser's session) and still clears it from the browser.
	r = e.do(t, "POST", "/ui/logout", "", map[string]string{"Cookie": cookieName + "=" + plain, "X-Forwarded-Proto": "https", "X-Swk-CSRF": "1"})
	if r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if _, ok := e.srv.web.validCookie(plain, time.Now()); !ok {
		t.Fatal("signing out over HTTPS revoked a swk_session cookie")
	}
}

func TestNoSniff(t *testing.T) {
	e := newWebEnv(t)
	rec := httptest.NewRecorder()
	securityHeaders(e.srv.Handler()).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts", nil))
	if rec.Code != 401 || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("API response: %d %v", rec.Code, rec.Header())
	}
}
