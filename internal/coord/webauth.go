// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/pki"
)

// Browser sessions: the web UI signs in with the admin token, or with a
// one-time login link minted by an authenticated client (`swyd ui-link`),
// and gets a signed cookie. Cookie-authenticated requests that change
// anything must also carry X-Swyd-CSRF: 1, which a cross-site page cannot
// add without a CORS preflight that this server never answers.

const (
	cookieName  = "swyd_session"
	cookieTTL   = 30 * 24 * time.Hour
	csrfHeader  = "X-Swyd-CSRF"
	uiKeyFile   = "ui.key"
	loginLinkTT = 15 * time.Minute
)

type webAuth struct {
	key []byte

	mu       sync.Mutex
	codes    map[string]time.Time // sha256(code) -> expiry
	failures []time.Time
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
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) < 32 {
		return nil, errors.New(path + " is too short; delete it to make a new one")
	}
	return &webAuth{key: key, codes: map[string]time.Time{}}, nil
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
	return now.Unix() < int64(binary.BigEndian.Uint64(payload))
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

// throttled limits failed sign-ins to 10 a minute.
func (a *webAuth) throttled(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := a.failures[:0]
	for _, t := range a.failures {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	a.failures = keep
	return len(a.failures) >= 10
}

func (a *webAuth) failed(now time.Time) {
	a.mu.Lock()
	a.failures = append(a.failures, now)
	a.mu.Unlock()
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
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
	var body struct {
		Token string `json:"token,omitempty"`
		Code  string `json:"code,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	now := time.Now()
	if s.web.throttled(now) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many failed sign-ins; wait a minute"))
		return
	}
	ok := (body.Token != "" && s.tokenOK(body.Token)) || (body.Code != "" && s.web.useCode(body.Code, now))
	if !ok {
		s.web.failed(now)
		writeErr(w, http.StatusUnauthorized, errors.New("that token or link is not valid (links work once, for 15 minutes)"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.web.newCookie(now), Path: "/", MaxAge: int(cookieTTL.Seconds()),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	s.logf("web sign-in from %s", r.RemoteAddr)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
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
