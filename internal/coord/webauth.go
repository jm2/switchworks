// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/paths"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/ulid"
)

// Browser sessions: the web UI signs in with the admin token or an API
// token, or with a one-time login link minted by an authenticated client
// (`swyd ui-link`), and gets a signed cookie carrying that credential's
// scope. Cookie-authenticated requests that change anything must also carry
// X-Swyd-CSRF: 1, which a cross-site page cannot add without a CORS
// preflight that this server never answers.

const (
	cookieName    = "swyd_session"
	cookieTTL     = 30 * 24 * time.Hour
	csrfHeader    = "X-Swyd-CSRF"
	uiKeyFile     = "ui.key"
	uiRevokedFile = "ui-revoked.json"
	loginLinkTT   = 15 * time.Minute
	maxFailures   = 10 // failed token sign-ins per client per minute
)

type webAuth struct {
	key        []byte
	revokePath string

	mu       sync.Mutex
	codes    map[string]loginCode   // sha256(code) -> expiry and grant
	failures map[string][]time.Time // client -> recent failed token sign-ins
	signins  map[string][]time.Time // credential -> recent sign-ins
	revoked  map[string]time.Time   // signed-out cookie id -> its expiry
}

// loadWebAuth reads (or creates) the cookie-signing key. Deleting ui.key
// signs every browser out.
func loadWebAuth(dir string) (*webAuth, error) {
	path := filepath.Join(dir, uiKeyFile)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := paths.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) < 32 {
		return nil, errors.New(path + " is too short; delete it to make a new one")
	}
	a := &webAuth{key: key, revokePath: filepath.Join(dir, uiRevokedFile), codes: map[string]loginCode{},
		failures: map[string][]time.Time{}, signins: map[string][]time.Time{}, revoked: map[string]time.Time{}}
	if b, err := os.ReadFile(a.revokePath); err == nil {
		_ = json.Unmarshal(b, &a.revoked)
	}
	return a, nil
}

func (a *webAuth) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(payload)
	return m.Sum(nil)
}

// A cookie's signed payload is its expiry (8 bytes, Unix seconds), a random
// id (16), then the grant: the scope (1 byte) and the id of the API token it
// came from (empty for the admin token). Cookies from before scoped tokens
// stop after the id; they were all minted with the admin token, so they
// stay admin.
const (
	cookieIDEnd     = 8 + 16
	legacyCookieLen = cookieIDEnd
)

// newCookie mints a cookie for g, expiring after cookieTTL or at notAfter
// (if set and sooner).
func (a *webAuth) newCookie(now time.Time, g grant, notAfter time.Time) string {
	exp := now.Add(cookieTTL)
	if !notAfter.IsZero() && notAfter.Before(exp) {
		exp = notAfter
	}
	payload := make([]byte, cookieIDEnd, cookieIDEnd+1+len(g.tokenID))
	binary.BigEndian.PutUint64(payload, uint64(exp.Unix()))
	_, _ = rand.Read(payload[8:cookieIDEnd])
	payload = append(payload, byte(g.scope))
	payload = append(payload, g.tokenID...)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(a.sign(payload))
}

// validCookie checks a cookie's signature, expiry and revocation, and
// returns the grant signed into it. Whether its API token still exists is
// the caller's check (Server.cookieGrant).
func (a *webAuth) validCookie(v string, now time.Time) (grant, bool) {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return grant{}, false
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p)
	got, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) < cookieIDEnd {
		return grant{}, false
	}
	if !hmac.Equal(got, a.sign(payload)) {
		return grant{}, false
	}
	if now.Unix() >= int64(binary.BigEndian.Uint64(payload)) {
		return grant{}, false
	}
	g := grant{scope: scopeAdmin}
	if len(payload) != legacyCookieLen {
		g = grant{scope: scope(payload[cookieIDEnd]), tokenID: string(payload[cookieIDEnd+1:])}
		if !g.scope.valid() || (g.tokenID != "" && !ulid.Valid(g.tokenID)) {
			return grant{}, false
		}
	}
	a.mu.Lock()
	_, out := a.revoked[cookieID(payload)]
	a.mu.Unlock()
	if out {
		return grant{}, false
	}
	return g, true
}

func cookieID(payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload[8:cookieIDEnd])
}

// revoke signs one cookie out for good, even if a copy of it survives.
func (a *webAuth) revoke(v string, now time.Time) {
	p, _, _ := strings.Cut(v, ".")
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || len(payload) < cookieIDEnd {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, exp := range a.revoked {
		if now.After(exp) {
			delete(a.revoked, id)
		}
	}
	a.revoked[cookieID(payload)] = time.Unix(int64(binary.BigEndian.Uint64(payload)), 0)
	if b, err := json.Marshal(a.revoked); err == nil {
		_ = paths.WriteFile(a.revokePath, b, 0o600)
	}
}

// loginCode is a pending one-time sign-in: when it expires, and the grant
// the cookie it buys will carry.
type loginCode struct {
	expires time.Time
	grant   grant
}

// newCode mints a one-time login code for g.
func (a *webAuth) newCode(now time.Time, ttl time.Duration, g grant) string {
	code := pki.RandomSecret(24)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, c := range a.codes {
		if now.After(c.expires) {
			delete(a.codes, k)
		}
	}
	// Each credential keeps at most maxCodesPerGrant pending links; a new
	// one replaces its oldest, so minting links cannot grow memory.
	var mine []string
	for k, c := range a.codes {
		if c.grant.tokenID == g.tokenID {
			mine = append(mine, k)
		}
	}
	if len(mine) >= maxCodesPerGrant {
		sort.Slice(mine, func(i, j int) bool { return a.codes[mine[i]].expires.Before(a.codes[mine[j]].expires) })
		for _, k := range mine[:len(mine)-maxCodesPerGrant+1] {
			delete(a.codes, k)
		}
	}
	a.codes[pki.HashSecret(code)] = loginCode{expires: now.Add(ttl), grant: g}
	return code
}

// maxCodesPerGrant bounds one credential's pending sign-in links.
const maxCodesPerGrant = 20

// maxSignInsPerMinute bounds sign-ins with one credential. Each sign-out
// is remembered until the cookie would have expired, so unbounded sign-ins
// would grow that list without limit.
const maxSignInsPerMinute = 20

// allowSignIn counts a sign-in with g and reports whether it is within
// maxSignInsPerMinute.
func (a *webAuth) allowSignIn(g grant, now time.Time) bool {
	key := "admin"
	if g.tokenID != "" {
		key = g.tokenID
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	recent := a.signins[key][:0]
	for _, t := range a.signins[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= maxSignInsPerMinute {
		a.signins[key] = recent
		return false
	}
	a.signins[key] = append(recent, now)
	return true
}

func (a *webAuth) useCode(code string, now time.Time) (grant, bool) {
	h := pki.HashSecret(code)
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.codes[h]
	delete(a.codes, h)
	if !ok || !now.Before(c.expires) {
		return grant{}, false
	}
	return c.grant, true
}

// checkToken compares a sign-in token, limiting each client to maxFailures
// failed attempts a minute. The check and the count happen under one lock,
// so concurrent guesses cannot slip past the limit.
func (a *webAuth) checkToken(client string, now time.Time, ok func() bool) (valid, throttled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.failures) > 10000 {
		a.failures = map[string][]time.Time{} // bounded memory under a flood
	}
	recent := a.failures[client][:0]
	for _, t := range a.failures[client] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= maxFailures {
		a.failures[client] = recent
		return false, true
	}
	if ok() {
		a.failures[client] = recent
		return true, false
	}
	a.failures[client] = append(recent, now)
	return false, false
}

// clientKey identifies a client for throttling: its address, or the address
// a reverse proxy on this machine forwarded for.
func clientKey(r *http.Request) string {
	h := ipBucket(remoteHost(r))
	if fromLocalProxy(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return ipBucket(strings.TrimSpace(parts[len(parts)-1]))
		}
	}
	return h
}

// fromLocalProxy reports whether a plain-HTTP request came from this
// machine, the only place a trusted reverse proxy runs (Config.HTTPListen).
func fromLocalProxy(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	ip := net.ParseIP(remoteHost(r))
	return ip != nil && ip.IsLoopback()
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || (fromLocalProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https")
}

// cookieGrant returns the grant of the request's session cookie, if it is
// valid and the API token it was minted from (if any) still exists.
func (s *Server) cookieGrant(r *http.Request) (grant, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return grant{}, false
	}
	now := time.Now()
	g, ok := s.web.validCookie(c.Value, now)
	if !ok || !s.grantLive(g, now) {
		return grant{}, false
	}
	return g, true
}

func (s *Server) cookieAuthed(r *http.Request) bool {
	_, ok := s.cookieGrant(r)
	return ok
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return
	}
	var body struct {
		Token string `json:"token,omitempty"`
		Code  string `json:"code,omitempty"`
	}
	if err := decodeBodyLimit(r, &body, 64<<10); err != nil {
		writeErr(w, 400, err)
		return
	}
	now := time.Now()
	var ok bool
	var g grant
	switch {
	case body.Code != "":
		// One-time codes are 192-bit and single-use: no throttle, so a
		// flood of bad tokens cannot block signing in with a link.
		g, ok = s.web.useCode(body.Code, now)
		ok = ok && s.grantLive(g, now) // its token may have been revoked since
	case body.Token != "":
		var throttled bool
		ok, throttled = s.web.checkToken(clientKey(r), now, func() bool {
			var valid bool
			g, valid = s.bearerGrant(body.Token, now)
			return valid
		})
		if throttled {
			writeErr(w, http.StatusTooManyRequests, errors.New("too many failed sign-ins; wait a minute"))
			return
		}
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("that token or link is not valid (links work once, for 15 minutes)"))
		return
	}
	if !s.web.allowSignIn(g, now) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many sign-ins with this token; wait a minute"))
		return
	}
	var notAfter time.Time
	if g.tokenID != "" {
		if t, found := s.store.APIToken(g.tokenID); found {
			notAfter = t.Expires // the cookie never outlives its token
		}
	}
	value := s.web.newCookie(now, g, notAfter)
	maxAge := cookieTTL
	if !notAfter.IsZero() && notAfter.Sub(now) < maxAge {
		maxAge = notAfter.Sub(now)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", MaxAge: max(int(maxAge.Seconds()), 1),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	s.logf("web sign-in from %s (%s)", clientKey(r), s.grantName(g))
	writeJSON(w, 200, map[string]any{"ok": true, "scope": g.scope.String()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if _, ok := s.web.validCookie(c.Value, time.Now()); ok {
			s.web.revoke(c.Value, time.Now())
			s.auth.recheck()
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleUILink mints a one-time sign-in link. The browser it signs in gets
// the caller's scope, or a lower one the caller asks for; never more.
func (s *Server) handleUILink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, err)
		return
	}
	g := grantFrom(r.Context())
	if body.Scope != "" {
		want, ok := parseScope(body.Scope)
		if !ok {
			writeErr(w, 400, errUnknownScope(body.Scope))
			return
		}
		if want > g.scope {
			writeErr(w, http.StatusForbidden, fmt.Errorf("a %s token cannot mint a %s sign-in link", g.scope, want))
			return
		}
		g.scope = want
	}
	now := time.Now()
	code := s.web.newCode(now, loginLinkTT, g)
	base := s.cfg.PublicUI
	if base == "" {
		base = "https://" + s.cfg.PublicAPI
	}
	// The code travels in the fragment, which browsers never send to the
	// server or in a Referer.
	writeJSON(w, 200, apiclient.UILink{URL: strings.TrimRight(base, "/") + "/login#code=" + code,
		ExpiresAt: now.Add(loginLinkTT).UTC(), Scope: g.scope.String()})
}
