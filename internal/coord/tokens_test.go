// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// samplePath turns a route pattern into a request: "GET /v1/x/{id}" ->
// GET /v1/x/x.
func samplePath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	return method, pathParam.ReplaceAllString(path, "x")
}

// Every authenticated route declares the scope it needs, and the mux
// resolves each one to its own declaration.
func TestEveryRouteDeclaresScope(t *testing.T) {
	e := newWebEnv(t)
	routes := e.srv.apiRoutes()
	mux, scopes := newAPIMux(routes)
	seen := map[string]bool{}
	for _, rt := range routes {
		if !rt.scope.valid() {
			t.Errorf("%s: no scope declared", rt.pattern)
		}
		if seen[rt.pattern] {
			t.Errorf("%s: declared twice", rt.pattern)
		}
		seen[rt.pattern] = true
		method, path := samplePath(rt.pattern)
		_, got := mux.Handler(httptest.NewRequest(method, path, nil))
		if got != rt.pattern {
			t.Errorf("%s: request %s %s matched %q", rt.pattern, method, path, got)
		}
		if requiredScope(scopes, got) != rt.scope {
			t.Errorf("%s: requires %s, declared %s", rt.pattern, requiredScope(scopes, got), rt.scope)
		}
	}
	if requiredScope(scopes, "GET /v1/not-declared") != scopeAdmin {
		t.Fatal("an undeclared pattern must need admin")
	}
	// A few routes whose scope is a security decision, pinned explicitly.
	for pattern, want := range map[string]scope{
		"GET /v1/stream":                scopeRead,
		"GET /v1/attachments/{sha256}":  scopeRead,
		"POST /v1/ui-links":             scopeRead,
		"POST /v1/sessions":             scopeOperate,
		"POST /v1/sessions/{id}/policy": scopeOperate,
		"DELETE /v1/hosts/{id}":         scopeAdmin,
		"POST /v1/enroll-tokens":        scopeAdmin,
		"POST /v1/tokens":               scopeAdmin,
		"GET /v1/tokens":                scopeAdmin,
		"DELETE /v1/tokens/{id}":        scopeAdmin,
		"POST /v1/tokens/{id}/owner":    scopeAdmin,
	} {
		if scopes[pattern] != want {
			t.Errorf("%s: scope %s, want %s", pattern, scopes[pattern], want)
		}
	}
}

// A route registered without a scope needs admin.
func TestUndeclaredScopeFailsClosed(t *testing.T) {
	e := newWebEnv(t)
	ok := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) }
	mux, scopes := newAPIMux([]apiRoute{{"GET /v1/forgotten", scopeNone, ok}})
	h := e.srv.requireAuth(mux, scopes)
	op := e.createToken(t, "op", "operate", 0)
	for _, c := range []struct {
		tok  string
		want int
	}{{op.Secret, 403}, {e.token, 200}} {
		req := httptest.NewRequest("GET", "/v1/forgotten", nil)
		req.Header.Set("Authorization", "Bearer "+c.tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
		}
	}
}

func (e *webEnv) createToken(t *testing.T, name, sc string, ttl time.Duration) apiclient.NewToken {
	t.Helper()
	b, _ := json.Marshal(apiclient.CreateTokenRequest{Name: name, Scope: sc, TTLSeconds: int64(ttl / time.Second)})
	r := e.do(t, "POST", "/v1/tokens", string(b), map[string]string{"Authorization": "Bearer " + e.token})
	if r.StatusCode != 201 {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("create token %s: %d %s", name, r.StatusCode, body)
	}
	var out apiclient.NewToken
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// request sends one request and returns its status and body, without
// waiting for a stream to end.
func (e *webEnv) request(t *testing.T, method, path string, hdr map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, e.http.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Every route, with every scope: allowed at or above its scope, 403 below.
func TestScopeEnforcementEveryRoute(t *testing.T) {
	e := newWebEnv(t)
	creds := map[scope]string{
		scopeRead:    e.createToken(t, "reader", "read", 0).Secret,
		scopeOperate: e.createToken(t, "operator", "operate", 0).Secret,
		scopeAdmin:   e.createToken(t, "deputy", "admin", 0).Secret,
	}
	for _, rt := range e.srv.apiRoutes() {
		method, path := samplePath(rt.pattern)
		for sc, tok := range creds {
			status, body := e.request(t, method, path, map[string]string{"Authorization": "Bearer " + tok})
			switch {
			case sc < rt.scope:
				if status != 403 || !strings.Contains(body, "needs a token with "+rt.scope.String()+" scope") {
					t.Errorf("%s with a %s token: %d %s, want 403", rt.pattern, sc, status, body)
				}
			case status == 401 || status == 403:
				t.Errorf("%s with a %s token: %d %s", rt.pattern, sc, status, body)
			}
		}
		// The admin token reaches everything.
		if status, body := e.request(t, method, path, map[string]string{"Authorization": "Bearer " + e.token}); status == 401 || status == 403 {
			t.Errorf("%s with the admin token: %d %s", rt.pattern, status, body)
		}
	}
}

// Representative routes per scope, end to end through real handlers.
func TestScopeTable(t *testing.T) {
	e := newWebEnv(t)
	toks := map[string]string{
		"read":    e.createToken(t, "r", "read", 0).Secret,
		"operate": e.createToken(t, "o", "operate", 0).Secret,
		"admin":   e.createToken(t, "a", "admin", 0).Secret,
	}
	cases := []struct {
		method, path, body string
		want               map[string]int
	}{
		{"GET", "/v1/hosts", "", map[string]int{"read": 200, "operate": 200, "admin": 200}},
		{"GET", "/v1/messages", "", map[string]int{"read": 200, "operate": 200, "admin": 200}},
		{"GET", "/v1/usage", "", map[string]int{"read": 200, "operate": 200, "admin": 200}},
		{"POST", "/v1/messages", `{"to":"user:dev","text":"hi"}`, map[string]int{"read": 403, "operate": 202, "admin": 202}},
		{"POST", "/v1/ui-links", `{}`, map[string]int{"read": 200, "operate": 200, "admin": 200}},
		{"POST", "/v1/enroll-tokens", `{"host":"devbox"}`, map[string]int{"read": 403, "operate": 403, "admin": 201}},
		{"GET", "/v1/tokens", "", map[string]int{"read": 403, "operate": 403, "admin": 200}},
		{"DELETE", "/v1/hosts/devbox", "", map[string]int{"read": 403, "operate": 403, "admin": 404}},
	}
	for _, c := range cases {
		for name, tok := range toks {
			r := e.do(t, c.method, c.path, c.body, map[string]string{"Authorization": "Bearer " + tok})
			if r.StatusCode != c.want[name] {
				b, _ := io.ReadAll(r.Body)
				t.Errorf("%s %s as %s: %d %s, want %d", c.method, c.path, name, r.StatusCode, b, c.want[name])
			}
		}
	}
}

func TestTokenLifecycle(t *testing.T) {
	e := newWebEnv(t)
	admin := map[string]string{"Authorization": "Bearer " + e.token}
	tok := e.createToken(t, "devbox-cli", "operate", 0)
	if tok.Secret == "" || !ulid.Valid(tok.ID) || tok.Scope != "operate" || !tok.Expires.IsZero() {
		t.Fatalf("created: %+v", tok)
	}
	// Bad requests.
	for body, want := range map[string]int{
		`{"name":"devbox-cli","scope":"read"}`:                409,
		`{"name":"Bad Name","scope":"read"}`:                  400,
		`{"name":"phone","scope":"root"}`:                     400,
		`{"name":"phone","scope":"read","ttl_seconds":-1}`:    400,
		`{"name":"phone","scope":"read","secret":"chosen"}`:   400,
		`{"name":"phone","scope":"read","ttl_seconds":3600}`:  201,
		`{"name":"phone2","scope":"admin","ttl_seconds":600}`: 201,
	} {
		if r := e.do(t, "POST", "/v1/tokens", body, admin); r.StatusCode != want {
			t.Errorf("create %s: %d, want %d", body, r.StatusCode, want)
		}
	}
	// The secret is stored only as a hash and never listed.
	r := e.do(t, "GET", "/v1/tokens", "", admin)
	raw, _ := io.ReadAll(r.Body)
	if strings.Contains(string(raw), tok.Secret) || strings.Contains(string(raw), pki.HashSecret(tok.Secret)) || strings.Contains(string(raw), "secret") {
		t.Fatalf("token list leaks a secret: %s", raw)
	}
	var list []apiclient.TokenInfo
	_ = json.Unmarshal(raw, &list)
	if len(list) != 3 || list[0].Name != "devbox-cli" || list[1].Name != "phone" || list[1].Expires.IsZero() {
		t.Fatalf("list: %+v", list)
	}
	if st, ok := e.srv.store.APIToken(tok.ID); !ok || st.Hash != pki.HashSecret(tok.Secret) {
		t.Fatalf("stored: %+v", st)
	}

	// The token works, and its use is recorded.
	bearer := map[string]string{"Authorization": "Bearer " + tok.Secret}
	if r := e.do(t, "GET", "/v1/hosts", "", bearer); r.StatusCode != 200 {
		t.Fatalf("token use: %d", r.StatusCode)
	}
	if st, _ := e.srv.store.APIToken(tok.ID); st.LastUsed.IsZero() {
		t.Fatal("last use not recorded")
	}
	// The admin token is unchanged and still admin.
	if r := e.do(t, "GET", "/v1/tokens", "", admin); r.StatusCode != 200 {
		t.Fatalf("admin token: %d", r.StatusCode)
	}

	// Revoke by name, then by id.
	if r := e.do(t, "DELETE", "/v1/tokens/devbox-cli", "", admin); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", bearer); r.StatusCode != 401 {
		t.Fatalf("revoked token: %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", "/v1/tokens/devbox-cli", "", admin); r.StatusCode != 404 {
		t.Fatalf("revoke again: %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", "/v1/tokens/"+list[1].ID, "", admin); r.StatusCode != 200 {
		t.Fatalf("revoke by id: %d", r.StatusCode)
	}
	// The name is free again.
	e.createToken(t, "devbox-cli", "read", 0)

	// Expired tokens stop working and drop out of the list.
	secret := pki.RandomSecret(32)
	old := store.APIToken{ID: ulid.New(), Name: "old", Scope: "admin", Hash: pki.HashSecret(secret),
		Created: time.Now().Add(-2 * time.Hour), Expires: time.Now().Add(-time.Hour)}
	if err := e.srv.store.AddAPIToken(old); err != nil {
		t.Fatal(err)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", map[string]string{"Authorization": "Bearer " + secret}); r.StatusCode != 401 {
		t.Fatalf("expired token: %d", r.StatusCode)
	}
	for _, tk := range e.srv.store.APITokens() {
		if tk.Name == "old" {
			t.Fatal("expired token listed")
		}
	}
}

func cookieHeader(t *testing.T, r *http.Response) map[string]string {
	t.Helper()
	if r.StatusCode != 200 || len(r.Cookies()) != 1 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("sign-in: %d %s", r.StatusCode, b)
	}
	ck := r.Cookies()[0]
	return map[string]string{"Cookie": ck.Name + "=" + ck.Value, "X-Swk-CSRF": "1"}
}

func (e *webEnv) uiLink(t *testing.T, tok, sc string) (int, string, string) {
	t.Helper()
	body := `{}`
	if sc != "" {
		body = `{"scope":"` + sc + `"}`
	}
	r := e.do(t, "POST", "/v1/ui-links", body, map[string]string{"Authorization": "Bearer " + tok})
	var l apiclient.UILink
	_ = json.NewDecoder(r.Body).Decode(&l)
	_, code, _ := strings.Cut(l.URL, "/login#code=")
	return r.StatusCode, code, l.Scope
}

func TestScopedWebSignIn(t *testing.T) {
	e := newWebEnv(t)
	msg := `{"to":"user:dev","text":"hi"}`
	reader := e.createToken(t, "phone", "read", 0)
	operator := e.createToken(t, "devbox-cli", "operate", time.Hour)

	// Signing in with a read token gives a read-only browser.
	readCookie := cookieHeader(t, e.do(t, "POST", "/ui/login", `{"token":"`+reader.Secret+`"}`, nil))
	if r := e.do(t, "GET", "/", "", readCookie); r.StatusCode != 200 {
		t.Fatalf("UI with a read cookie: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", readCookie); r.StatusCode != 200 {
		t.Fatalf("read with a read cookie: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/v1/messages", msg, readCookie); r.StatusCode != 403 {
		t.Fatalf("write with a read cookie: %d", r.StatusCode)
	}

	// An operate token's cookie can write, but not administer, and does
	// not outlive the token.
	r := e.do(t, "POST", "/ui/login", `{"token":"`+operator.Secret+`"}`, nil)
	if ck := r.Cookies(); len(ck) == 1 && ck[0].MaxAge > 3600 {
		t.Fatalf("cookie lifetime %ds outlives its token", ck[0].MaxAge)
	}
	opCookie := cookieHeader(t, r)
	if r := e.do(t, "POST", "/v1/messages", msg, opCookie); r.StatusCode != 202 {
		t.Fatalf("write with an operate cookie: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/tokens", "", opCookie); r.StatusCode != 403 {
		t.Fatalf("admin with an operate cookie: %d", r.StatusCode)
	}

	// One-time links carry the minting token's scope, never more.
	if st, _, sc := e.uiLink(t, operator.Secret, ""); st != 200 || sc != "operate" {
		t.Fatalf("operate link: %d %s", st, sc)
	}
	if st, _, _ := e.uiLink(t, operator.Secret, "admin"); st != 403 {
		t.Fatalf("operate token minting an admin link: %d", st)
	}
	if st, _, _ := e.uiLink(t, reader.Secret, "operate"); st != 403 {
		t.Fatalf("read token minting an operate link: %d", st)
	}
	st, code, sc := e.uiLink(t, reader.Secret, "")
	if st != 200 || sc != "read" {
		t.Fatalf("read link: %d %s", st, sc)
	}
	linkCookie := cookieHeader(t, e.do(t, "POST", "/ui/login", `{"code":"`+code+`"}`, nil))
	if r := e.do(t, "POST", "/v1/messages", msg, linkCookie); r.StatusCode != 403 {
		t.Fatalf("write with a read link's cookie: %d", r.StatusCode)
	}
	// An operate token can hand out a read-only link.
	_, code, sc = e.uiLink(t, operator.Secret, "read")
	downCookie := cookieHeader(t, e.do(t, "POST", "/ui/login", `{"code":"`+code+`"}`, nil))
	if r := e.do(t, "POST", "/v1/messages", msg, downCookie); sc != "read" || r.StatusCode != 403 {
		t.Fatalf("operate token's read link: %s %d", sc, r.StatusCode)
	}
	// A browser can mint links too, at its own scope.
	if r := e.do(t, "POST", "/v1/ui-links", `{"scope":"operate"}`, linkCookie); r.StatusCode != 403 {
		t.Fatalf("read browser minting an operate link: %d", r.StatusCode)
	}

	// Revoking a token signs out every browser it reached, and voids its
	// unused links.
	_, pending, _ := e.uiLink(t, reader.Secret, "")
	if r := e.do(t, "DELETE", "/v1/tokens/phone", "", map[string]string{"Authorization": "Bearer " + e.token}); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	for name, c := range map[string]map[string]string{"token sign-in": readCookie, "link": linkCookie} {
		if r := e.do(t, "GET", "/v1/hosts", "", c); r.StatusCode != 401 {
			t.Errorf("%s cookie after revoke: %d", name, r.StatusCode)
		}
		if r := e.do(t, "GET", "/", "", c); r.StatusCode != 302 {
			t.Errorf("%s cookie on the UI after revoke: %d", name, r.StatusCode)
		}
	}
	if r := e.do(t, "POST", "/ui/login", `{"code":"`+pending+`"}`, nil); r.StatusCode != 401 {
		t.Fatalf("link from a revoked token: %d", r.StatusCode)
	}
	if r := e.do(t, "GET", "/v1/hosts", "", opCookie); r.StatusCode != 200 {
		t.Fatalf("another token's cookie after revoke: %d", r.StatusCode)
	}

	// The admin token signs in as admin, as before.
	adminCookie := cookieHeader(t, e.do(t, "POST", "/ui/login", `{"token":"`+e.token+`"}`, nil))
	if r := e.do(t, "GET", "/v1/tokens", "", adminCookie); r.StatusCode != 200 {
		t.Fatalf("admin cookie: %d", r.StatusCode)
	}
	if st, _, sc := e.uiLink(t, e.token, ""); st != 200 || sc != "admin" {
		t.Fatalf("admin link: %d %s", st, sc)
	}
}

// A grant counts only up to its token's stored scope. Cookies and links are
// minted at or below it anyway; this is defense in depth.
func TestGrantLiveChecksScope(t *testing.T) {
	e := newWebEnv(t)
	tok := e.createToken(t, "phone", "read", 0)
	now := time.Now()
	if e.srv.grantLive(grant{scope: scopeOperate, tokenID: tok.ID}, now) {
		t.Fatal("a grant above its token's scope is live")
	}
	if !e.srv.grantLive(grant{scope: scopeRead, tokenID: tok.ID}, now) {
		t.Fatal("a grant at its token's scope is not live")
	}
	if e.srv.grantLive(grant{scope: scopeRead, tokenID: ulid.New()}, now) {
		t.Fatal("a grant from an unknown token is live")
	}
	if e.srv.grantLive(grant{}, now) {
		t.Fatal("an empty grant is live")
	}
}

// Revoking a token, or signing out, ends streams opened before it.
func TestRevocationEndsStreams(t *testing.T) {
	e := newWebEnv(t)
	admin := map[string]string{"Authorization": "Bearer " + e.token}
	ends := func(name string, r *http.Response, revoke func()) {
		t.Helper()
		if r.StatusCode != 200 {
			t.Fatalf("%s: stream: %d", name, r.StatusCode)
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, r.Body)
			close(done)
		}()
		select {
		case <-done:
			t.Fatalf("%s: stream ended before the revocation", name)
		case <-time.After(200 * time.Millisecond):
		}
		revoke()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: stream still open after the revocation", name)
		}
	}

	tok := e.createToken(t, "phone", "read", 0)
	ends("token", e.do(t, "GET", "/v1/stream", "", map[string]string{"Authorization": "Bearer " + tok.Secret}), func() {
		if r := e.do(t, "DELETE", "/v1/tokens/phone", "", admin); r.StatusCode != 200 {
			t.Fatalf("revoke: %d", r.StatusCode)
		}
	})

	tok = e.createToken(t, "tablet", "read", 0)
	ck := cookieHeader(t, e.do(t, "POST", "/ui/login", `{"token":"`+tok.Secret+`"}`, nil))
	ends("cookie", e.do(t, "GET", "/v1/stream", "", ck), func() {
		if r := e.do(t, "POST", "/ui/logout", "", ck); r.StatusCode != 200 {
			t.Fatalf("sign out: %d", r.StatusCode)
		}
	})
}

// An admin-scope token that is not the owner's manages tokens, but cannot
// make an owner token, mark one (itself included) or revoke one: that
// takes the admin token or an owner token.
func TestOwnerTokensNeedAnOwner(t *testing.T) {
	e := newWebEnv(t)
	admin := map[string]string{"Authorization": "Bearer " + e.token}
	ops := e.createToken(t, "ops", "admin", 0)
	asOps := map[string]string{"Authorization": "Bearer " + ops.Secret}

	if r := e.do(t, "POST", "/v1/tokens", `{"name":"mine","scope":"admin","owner":true}`, asOps); r.StatusCode != 403 {
		t.Fatalf("non-owner minted an owner token: %d", r.StatusCode)
	}
	if r := e.do(t, "POST", "/v1/tokens/ops/owner", `{"owner":true}`, asOps); r.StatusCode != 403 {
		t.Fatalf("non-owner marked itself owner: %d", r.StatusCode)
	}
	if tk, _ := e.srv.store.APIToken(ops.ID); tk.Owner {
		t.Fatal("ops became an owner")
	}
	// Plain tokens are still its to make and revoke.
	if r := e.do(t, "POST", "/v1/tokens", `{"name":"plain","scope":"read"}`, asOps); r.StatusCode != 201 {
		t.Fatalf("non-owner making a plain token: %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", "/v1/tokens/plain", "", asOps); r.StatusCode != 200 {
		t.Fatalf("non-owner revoking a plain token: %d", r.StatusCode)
	}

	// The admin token makes owners; an owner token may too.
	r := e.do(t, "POST", "/v1/tokens", `{"name":"boss","scope":"admin","owner":true}`, admin)
	var boss apiclient.NewToken
	if err := json.NewDecoder(r.Body).Decode(&boss); r.StatusCode != 201 || err != nil || !boss.Owner {
		t.Fatalf("admin minting an owner token: %d %+v", r.StatusCode, boss)
	}
	if r := e.do(t, "DELETE", "/v1/tokens/boss", "", asOps); r.StatusCode != 403 {
		t.Fatalf("non-owner revoked an owner token: %d", r.StatusCode)
	}
	asBoss := map[string]string{"Authorization": "Bearer " + boss.Secret}
	if r := e.do(t, "POST", "/v1/tokens/ops/owner", `{"owner":true}`, asBoss); r.StatusCode != 200 {
		t.Fatalf("owner token marking: %d", r.StatusCode)
	}
	if tk, _ := e.srv.store.APIToken(ops.ID); !tk.Owner {
		t.Fatal("ops not marked")
	}
	if r := e.do(t, "POST", "/v1/tokens/ops/owner", `{"owner":false}`, admin); r.StatusCode != 200 {
		t.Fatalf("admin unmarking: %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", "/v1/tokens/boss", "", admin); r.StatusCode != 200 {
		t.Fatalf("admin revoking an owner token: %d", r.StatusCode)
	}
}
