// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/paths"
	"github.com/jm2/switchyard/internal/pki"
)

// Browser sessions: the web UI signs in with the admin token, or with a
// one-time login link minted by an authenticated client (`swyd ui-link`),
// and gets a signed cookie. Cookie-authenticated requests that change
// anything must also carry X-Swyd-CSRF: 1, which a cross-site page cannot
// add without a CORS preflight that this server never answers.

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
	codes    map[string]time.Time   // sha256(code) -> expiry
	failures map[string][]time.Time // client -> recent failed token sign-ins
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
	a := &webAuth{key: key, revokePath: filepath.Join(dir, uiRevokedFile), codes: map[string]time.Time{},
		failures: map[string][]time.Time{}, revoked: map[string]time.Time{}}
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

func (a *webAuth) newCookie(now time.Time) string {
	payload := make([]byte, 8+16)
	binary.BigEndian.PutUint64(payload, uint64(now.Add(cookieTTL).Unix()))
	_, _ = rand.Read(payload[8:])
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(a.sign(payload))
}

func (a *webAuth) validCookie(v string, now time.Time) bool {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p)
	got, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) != 24 {
		return false
	}
	if !hmac.Equal(got, a.sign(payload)) {
		return false
	}
	if now.Unix() >= int64(binary.BigEndian.Uint64(payload)) {
		return false
	}
	a.mu.Lock()
	_, out := a.revoked[cookieID(payload)]
	a.mu.Unlock()
	return !out
}

func cookieID(payload []byte) string { return base64.RawURLEncoding.EncodeToString(payload[8:]) }

// revoke signs one cookie out for good, even if a copy of it survives.
func (a *webAuth) revoke(v string, now time.Time) {
	p, _, _ := strings.Cut(v, ".")
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || len(payload) != 24 {
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

// newCode mints a one-time login code.
func (a *webAuth) newCode(now time.Time, ttl time.Duration) string {
	code := pki.RandomSecret(24)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, exp := range a.codes {
		if now.After(exp) {
			delete(a.codes, k)
		}
	}
	a.codes[pki.HashSecret(code)] = now.Add(ttl)
	return code
}

func (a *webAuth) useCode(code string, now time.Time) bool {
	h := pki.HashSecret(code)
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.codes[h]
	delete(a.codes, h)
	return ok && now.Before(exp)
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

func (s *Server) cookieAuthed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.web.validCookie(c.Value, time.Now())
}

func (s *Server) tokenOK(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(pki.HashSecret(tok)), []byte(s.cfg.AdminTokenHash)) == 1
}

// requireAuth accepts the admin bearer token, or a browser session cookie
// (plus the CSRF header on anything but reads).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if s.tokenOK(tok) {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		if s.cookieAuthed(r) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
				writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
	})
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
	switch {
	case body.Code != "":
		// One-time codes are 192-bit and single-use: no throttle, so a
		// flood of bad tokens cannot block signing in with a link.
		ok = s.web.useCode(body.Code, now)
	case body.Token != "":
		var throttled bool
		ok, throttled = s.web.checkToken(clientKey(r), now, func() bool { return s.tokenOK(body.Token) })
		if throttled {
			writeErr(w, http.StatusTooManyRequests, errors.New("too many failed sign-ins; wait a minute"))
			return
		}
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("that token or link is not valid (links work once, for 15 minutes)"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.web.newCookie(now), Path: "/", MaxAge: int(cookieTTL.Seconds()),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	s.logf("web sign-in from %s", clientKey(r))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return
	}
	if c, err := r.Cookie(cookieName); err == nil && s.web.validCookie(c.Value, time.Now()) {
		s.web.revoke(c.Value, time.Now())
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleUILink(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	code := s.web.newCode(now, loginLinkTT)
	base := s.cfg.PublicUI
	if base == "" {
		base = "https://" + s.cfg.PublicAPI
	}
	// The code travels in the fragment, which browsers never send to the
	// server or in a Referer.
	writeJSON(w, 200, apiclient.UILink{URL: strings.TrimRight(base, "/") + "/login#code=" + code, ExpiresAt: now.Add(loginLinkTT).UTC()})
}
