// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
)

// API access. Every authenticated request carries a grant: a scope, and the
// API token it came from ("" for the admin token in coordinator.json).
// Bearer tokens, cookies and one-time sign-in links all resolve to one, and
// requireAuth checks it against the scope the matched route declares.

// scope is what a credential may do. Each scope includes the ones below it.
type scope byte

const (
	scopeNone scope = iota // never granted: a route left at this needs admin
	scopeRead
	scopeOperate
	scopeAdmin
)

func (s scope) valid() bool { return s >= scopeRead && s <= scopeAdmin }

func (s scope) String() string {
	switch s {
	case scopeRead:
		return apiclient.ScopeRead
	case scopeOperate:
		return apiclient.ScopeOperate
	case scopeAdmin:
		return apiclient.ScopeAdmin
	}
	return "none"
}

func parseScope(s string) (scope, bool) {
	switch s {
	case apiclient.ScopeRead:
		return scopeRead, true
	case apiclient.ScopeOperate:
		return scopeOperate, true
	case apiclient.ScopeAdmin:
		return scopeAdmin, true
	}
	return scopeNone, false
}

func errUnknownScope(s string) error {
	return fmt.Errorf("unknown scope %q (read, operate, admin)", s)
}

type grant struct {
	scope   scope
	tokenID string // "" for the admin token
}

type grantKey struct{}

func withGrant(ctx context.Context, g grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// grantFrom returns the request's grant (requireAuth sets it).
func grantFrom(ctx context.Context) grant {
	g, _ := ctx.Value(grantKey{}).(grant)
	return g
}

// apiRoute is one authenticated endpoint.
type apiRoute struct {
	pattern string
	scope   scope
	handler http.HandlerFunc
}

// newAPIMux registers routes and returns, for each pattern, the scope it
// needs.
func newAPIMux(routes []apiRoute) (*http.ServeMux, map[string]scope) {
	mux := http.NewServeMux()
	scopes := make(map[string]scope, len(routes))
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, rt.handler)
		scopes[rt.pattern] = rt.scope
	}
	return mux, scopes
}

// requiredScope is the scope a matched pattern needs. Fail closed: a
// pattern without a declared scope needs admin.
func requiredScope(scopes map[string]scope, pattern string) scope {
	if sc, ok := scopes[pattern]; ok && sc.valid() {
		return sc
	}
	return scopeAdmin
}

// bearerGrant resolves a bearer token: the admin token, or an unexpired API
// token.
func (s *Server) bearerGrant(tok string, now time.Time) (grant, bool) {
	if tok == "" {
		return grant{}, false
	}
	h := pki.HashSecret(tok)
	if subtle.ConstantTimeCompare([]byte(h), []byte(s.cfg.AdminTokenHash)) == 1 {
		return grant{scope: scopeAdmin}, true
	}
	t, ok := s.store.APITokenByHash(h)
	if !ok {
		return grant{}, false
	}
	sc, ok := parseScope(t.Scope)
	if !ok {
		return grant{}, false
	}
	s.store.TouchAPIToken(t.ID, now)
	return grant{scope: sc, tokenID: t.ID}, true
}

// grantLive reports whether a grant from a cookie or link still stands: the
// admin token's always do; an API token's only while that token exists,
// has not expired and still has at least that scope.
func (s *Server) grantLive(g grant, now time.Time) bool {
	if !g.scope.valid() {
		return false
	}
	if g.tokenID == "" {
		return true
	}
	t, ok := s.store.APIToken(g.tokenID)
	if !ok || t.Expired(now) {
		return false
	}
	sc, ok := parseScope(t.Scope)
	if !ok || g.scope > sc {
		return false
	}
	s.store.TouchAPIToken(t.ID, now)
	return true
}

// credential names the credential behind a request's grant, as messages
// from people carry it ("admin", or "token:<name>"), and whether it is the
// owner's: the admin token is, an API token if marked so (live, so marking
// or unmarking one applies to the browsers it signed in too). ok is false
// outside an authenticated request, such as an agent's fleet operation.
func (s *Server) credential(ctx context.Context) (via string, owner, ok bool) {
	g := grantFrom(ctx)
	if !g.scope.valid() {
		return "", false, false
	}
	if g.tokenID == "" {
		return "admin", true, true
	}
	if t, found := s.store.APIToken(g.tokenID); found {
		return "token:" + t.Name, t.Owner, true
	}
	return "token:" + g.tokenID, false, true
}

// grantName names a grant's credential for logs.
func (s *Server) grantName(g grant) string {
	if g.tokenID == "" {
		return "admin token"
	}
	if t, ok := s.store.APIToken(g.tokenID); ok {
		return fmt.Sprintf("token %s, %s", t.Name, g.scope)
	}
	return "token " + g.tokenID
}

// requireAuth accepts a bearer token (the admin token or an API token), or
// a browser session cookie (plus the CSRF header on anything but reads),
// and refuses requests beyond the credential's scope with 403.
func (s *Server) requireAuth(api *http.ServeMux, scopes map[string]scope) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		var g grant
		var still func(time.Time) bool // whether the credential is still good
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			// Bad bearer tokens count against the same per-client limit as
			// bad sign-ins, so the API is no faster a way to guess one.
			valid, throttled := s.web.checkToken(clientKey(r), now, func() bool {
				var ok bool
				g, ok = s.bearerGrant(tok, now)
				return ok
			})
			if throttled {
				writeErr(w, http.StatusTooManyRequests, errors.New("too many failed attempts with a bad token; wait a minute"))
				return
			}
			if !valid {
				writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
				return
			}
			still = func(t time.Time) bool { return s.grantLive(g, t) }
		} else if value, cg, ok := s.sessionCookie(r); ok {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
				writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
				return
			}
			g = cg
			still = func(t time.Time) bool {
				cg, ok := s.web.validCookie(value, t)
				return ok && cg == g && s.grantLive(g, t)
			}
		} else {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		// A request that matches no route only gets the mux's 404 or 405.
		if _, pattern := api.Handler(r); pattern != "" {
			if need := requiredScope(scopes, pattern); g.scope < need {
				writeErr(w, http.StatusForbidden, fmt.Errorf("%s needs a token with %s scope; this one has %s", pattern, need, g.scope))
				return
			}
		}
		// Streams and long waits end when the credential stops being good.
		ctx, done := s.auth.watch(withGrant(r.Context(), g), still)
		defer done()
		api.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tokenInfo(t store.APIToken) apiclient.TokenInfo {
	return apiclient.TokenInfo{ID: t.ID, Name: t.Name, Scope: t.Scope, Created: t.Created, LastUsed: t.LastUsed, Expires: t.Expires, Owner: t.Owner}
}

// maxTokenTTL bounds an expiring token's lifetime (10 years), so the
// expiry cannot overflow.
const maxTokenTTL = 10 * 365 * 24 * time.Hour

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var body apiclient.CreateTokenRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := model.ValidateName("token", body.Name); err != nil {
		writeErr(w, 400, err)
		return
	}
	if ulid.Valid(strings.ToUpper(body.Name)) {
		// `swk token rm` takes a name or an id: a name shaped like an id
		// could revoke the wrong token.
		writeErr(w, 400, errors.New("a token name cannot look like a token id"))
		return
	}
	if _, ok := parseScope(body.Scope); !ok {
		writeErr(w, 400, errUnknownScope(body.Scope))
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	if body.TTLSeconds < 0 || ttl > maxTokenTTL {
		writeErr(w, 400, errors.New("ttl_seconds must be between 0 (no expiry) and ten years"))
		return
	}
	now := time.Now().UTC()
	secret := pki.RandomSecret(32)
	t := store.APIToken{ID: ulid.New(), Name: body.Name, Scope: body.Scope, Hash: pki.HashSecret(secret), Created: now.Truncate(time.Second),
		Owner: body.Owner}
	if ttl > 0 {
		t.Expires = now.Add(ttl).Truncate(time.Second)
	}
	if err := s.store.AddAPIToken(t); err != nil {
		status := 500
		if errors.Is(err, store.ErrNameInUse) {
			status = 409
		}
		writeErr(w, status, err)
		return
	}
	s.logf("created API token %s (%s scope%s, by %s)", t.Name, t.Scope, map[bool]string{true: ", owner"}[t.Owner], s.grantName(grantFrom(r.Context())))
	writeJSON(w, 201, apiclient.NewToken{TokenInfo: tokenInfo(t), Secret: secret})
}

// handleTokenOwner marks a token (by id or name) as the owner's, or not.
func (s *Server) handleTokenOwner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Owner bool `json:"owner"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	ref := r.PathValue("id")
	for _, t := range s.store.APITokens() {
		if t.ID != ref && t.Name != ref {
			continue
		}
		if err := s.store.SetAPITokenOwner(t.ID, body.Owner); err != nil {
			status := 500
			if errors.Is(err, store.ErrNotFound) {
				status = 404
			}
			writeErr(w, status, err)
			return
		}
		s.logf("token %s: owner %v (by %s)", t.Name, body.Owner, s.grantName(grantFrom(r.Context())))
		t.Owner = body.Owner
		writeJSON(w, 200, tokenInfo(t))
		return
	}
	writeErr(w, 404, fmt.Errorf("token %q: %w", ref, store.ErrNotFound))
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	out := []apiclient.TokenInfo{}
	for _, t := range s.store.APITokens() {
		out = append(out, tokenInfo(t))
	}
	writeJSON(w, 200, out)
}

// handleDeleteToken revokes a token by id or name. Cookies and pending
// sign-in links minted from it stop working at once (grantLive).
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	var target store.APIToken
	found := false
	for _, t := range s.store.APITokens() {
		if t.ID == ref || t.Name == ref {
			target, found = t, true
			break
		}
	}
	if !found {
		writeErr(w, 404, fmt.Errorf("token %q: %w", ref, store.ErrNotFound))
		return
	}
	if err := s.store.DeleteAPIToken(target.ID); err != nil {
		status := 500
		if errors.Is(err, store.ErrNotFound) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	s.auth.recheck()
	s.logf("revoked API token %s (%s)", target.Name, target.ID)
	writeJSON(w, 200, map[string]any{"ok": true, "id": target.ID, "name": target.Name})
}
