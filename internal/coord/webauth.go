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

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/ulid"
)

// Browser sessions: the web UI signs in with the admin token or an API
// token, or with a one-time login link minted by an authenticated client
// (`swk ui-link`), and gets a signed cookie carrying that credential's
// scope. Cookie-authenticated requests that change anything must also carry
// X-Swk-CSRF: 1, which a cross-site page cannot add without a CORS
// preflight that this server never answers.

const (
	// Over HTTPS the cookie is __Host-swk_session: browsers then refuse it
	// unless it is Secure, for Path=/ and set without a Domain, so another
	// host or plain-HTTP page cannot plant or shadow it. Plain HTTP (and
	// cookies from before the rename) use swk_session.
	cookieName       = "swk_session"
	secureCookieName = "__Host-swk_session"
	cookieTTL        = 30 * 24 * time.Hour
	csrfHeader       = "X-Swk-CSRF"
	uiKeyFile        = "ui.key"
	uiRevokedFile    = "ui-revoked.json"
	loginLinkTT      = 15 * time.Minute
	maxFailures      = 10 // failed token sign-ins per client per minute
	// maxClients bounds the clients tracked for failures; past it the
	// longest-quiet ones are forgotten first.
	maxClients = 10000
)

type webAuth struct {
	key        []byte
	revokePath string
	notice     string // what loading had to repair, for the log

	saveMu   sync.Mutex // orders writes of revokePath, outside mu
	mu       sync.Mutex
	codes    map[string]loginCode   // sha256(code) -> expiry and grant
	failures map[string][]time.Time // client -> recent failed token sign-ins
	signins  map[string][]time.Time // credential -> recent sign-ins
	revoked  revocations
}

// revocations are the sign-outs: cookies signed out one by one, and per
// credential a time before which every cookie it signed in is out. A
// credential keeps at most maxRevokedPerCred single sign-outs; past that,
// its oldest are folded into its cutoff, so the list stays bounded by the
// number of credentials however often a browser signs in and out. Folding
// signs out the credential's other browsers that signed in before the
// folded cookie: never in ordinary use (it takes that many sign-outs within
// one cookie lifetime), only when a credential is used to churn sessions.
type revocations struct {
	Cookies map[string]revokedCookie `json:"cookies"`          // cookie id ->
	Before  map[string]int64         `json:"before,omitempty"` // credential -> cutoff, Unix ns
}

type revokedCookie struct {
	Cred    string `json:"cred"`
	Issued  int64  `json:"issued"`  // Unix ns
	Expires int64  `json:"expires"` // Unix s
}

// maxRevokedPerCred bounds one credential's single sign-outs.
const maxRevokedPerCred = 256

// legacyCred marks sign-outs read from the old file format, which did not
// record the credential. They are never folded and age out within
// cookieTTL.
const legacyCred = "legacy"

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
		failures: map[string][]time.Time{}, signins: map[string][]time.Time{},
		revoked: revocations{Cookies: map[string]revokedCookie{}, Before: map[string]int64{}}}
	b, err := os.ReadFile(a.revokePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if perr := a.revoked.parse(b); perr != nil {
			// Starting without the list would let signed-out cookies back
			// in, and refusing to start would take the fleet down over a
			// sign-out list: a new key signs every browser out instead.
			aside := fmt.Sprintf("%s.corrupt-%d", a.revokePath, time.Now().Unix())
			if err := os.Rename(a.revokePath, aside); err != nil {
				return nil, fmt.Errorf("%s: %w (and could not move it aside: %v)", a.revokePath, perr, err)
			}
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := paths.WriteFile(path, key, 0o600); err != nil {
				return nil, err
			}
			a.key = key
			a.revoked = revocations{Cookies: map[string]revokedCookie{}, Before: map[string]int64{}}
			a.notice = fmt.Sprintf("%s could not be read (%v): moved it to %s and replaced %s, so every browser must sign in again", a.revokePath, perr, aside, uiKeyFile)
		}
	}
	return a, nil
}

// parse reads the sign-out file: the current format, or the old one, a map
// of cookie id to expiry.
func (v *revocations) parse(b []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	if _, ok := top["cookies"]; ok {
		var n revocations
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		for id, c := range n.Cookies {
			v.Cookies[id] = c
		}
		for cred, t := range n.Before {
			v.Before[cred] = t
		}
		return nil
	}
	var old map[string]time.Time
	if err := json.Unmarshal(b, &old); err != nil {
		return err
	}
	for id, exp := range old {
		v.Cookies[id] = revokedCookie{Cred: legacyCred, Expires: exp.Unix()}
	}
	return nil
}

func (a *webAuth) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(payload)
	return m.Sum(nil)
}

// A cookie's signed payload is its expiry (8 bytes, Unix seconds), a random
// id (16), then the grant: the scope (1 byte) and the id of the API token it
// came from (empty for the admin token). Since sign-out cutoffs, the scope
// byte carries cookieIssuedFlag and the 8 bytes after it hold when the
// cookie was minted (Unix ns). Cookies from before scoped tokens stop after
// the id; they were all minted with the admin token, so they stay admin.
const (
	cookieIDEnd      = 8 + 16
	legacyCookieLen  = cookieIDEnd
	cookieIssuedFlag = 0x80
)

// newCookie mints a cookie for g, expiring after cookieTTL or at notAfter
// (if set and sooner).
func (a *webAuth) newCookie(now time.Time, g grant, notAfter time.Time) string {
	exp := now.Add(cookieTTL)
	if !notAfter.IsZero() && notAfter.Before(exp) {
		exp = notAfter
	}
	payload := make([]byte, cookieIDEnd, cookieIDEnd+1+8+len(g.tokenID))
	binary.BigEndian.PutUint64(payload, uint64(exp.Unix()))
	_, _ = rand.Read(payload[8:cookieIDEnd])
	payload = append(payload, byte(g.scope)|cookieIssuedFlag)
	payload = binary.BigEndian.AppendUint64(payload, uint64(now.UnixNano()))
	payload = append(payload, g.tokenID...)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(a.sign(payload))
}

// cookie is what a signed cookie says.
type cookie struct {
	id      string
	expires int64 // Unix s
	issued  int64 // Unix ns
	grant   grant
}

// cred names the credential behind a grant, for sign-in limits and
// sign-out cutoffs.
func (g grant) cred() string {
	if g.tokenID == "" {
		return "admin"
	}
	return g.tokenID
}

// parseCookie checks a cookie's signature and decodes it.
func (a *webAuth) parseCookie(v string) (cookie, bool) {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return cookie{}, false
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p)
	got, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) < cookieIDEnd {
		return cookie{}, false
	}
	if !hmac.Equal(got, a.sign(payload)) {
		return cookie{}, false
	}
	c := cookie{id: enc.EncodeToString(payload[8:cookieIDEnd]), expires: int64(binary.BigEndian.Uint64(payload)),
		grant: grant{scope: scopeAdmin}}
	// Cookies that do not say when they were minted count as minted a full
	// lifetime before they expire: never later than they were.
	c.issued = time.Unix(c.expires, 0).Add(-cookieTTL).UnixNano()
	if len(payload) != legacyCookieLen {
		b, rest := payload[cookieIDEnd], payload[cookieIDEnd+1:]
		if b&cookieIssuedFlag != 0 {
			if len(rest) < 8 {
				return cookie{}, false
			}
			c.issued, rest = int64(binary.BigEndian.Uint64(rest)), rest[8:]
		}
		c.grant = grant{scope: scope(b &^ cookieIssuedFlag), tokenID: string(rest)}
		if !c.grant.scope.valid() || (c.grant.tokenID != "" && !ulid.Valid(c.grant.tokenID)) {
			return cookie{}, false
		}
	}
	return c, true
}

// validCookie checks a cookie's signature, expiry and revocation, and
// returns the grant signed into it. Whether its API token still exists is
// the caller's check (Server.cookieGrant).
func (a *webAuth) validCookie(v string, now time.Time) (grant, bool) {
	c, ok := a.parseCookie(v)
	if !ok || now.Unix() >= c.expires {
		return grant{}, false
	}
	a.mu.Lock()
	_, out := a.revoked.Cookies[c.id]
	if before, ok := a.revoked.Before[c.grant.cred()]; ok && c.issued <= before {
		out = true
	}
	a.mu.Unlock()
	if out {
		return grant{}, false
	}
	return c.grant, true
}

// revoke signs one cookie out for good, even if a copy of it survives. The
// sign-out holds in memory at once; the error says it could not be saved,
// so it would not survive a restart.
func (a *webAuth) revoke(v string, now time.Time) error {
	c, ok := a.parseCookie(v)
	if !ok {
		return nil
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	a.revoked.add(c, now)
	b, err := json.Marshal(a.revoked)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	// Written outside mu, so a slow disk does not hold up every request's
	// cookie check; saveMu keeps a stale snapshot from landing last.
	return paths.WriteFile(a.revokePath, b, 0o600)
}

// add records c's sign-out, dropping what has expired and folding the
// credential's oldest sign-outs into its cutoff past maxRevokedPerCred.
func (v *revocations) add(c cookie, now time.Time) {
	for id, r := range v.Cookies {
		if now.Unix() >= r.Expires {
			delete(v.Cookies, id)
		}
	}
	for cred, t := range v.Before {
		if time.Unix(0, t).Add(cookieTTL).Before(now) {
			delete(v.Before, cred) // every cookie minted by then has expired
		}
	}
	cred := c.grant.cred()
	v.Cookies[c.id] = revokedCookie{Cred: cred, Issued: c.issued, Expires: c.expires}
	var mine []string
	for id, r := range v.Cookies {
		if r.Cred == cred {
			mine = append(mine, id)
		}
	}
	if len(mine) <= maxRevokedPerCred {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return v.Cookies[mine[i]].Issued < v.Cookies[mine[j]].Issued })
	cut := v.Cookies[mine[len(mine)-maxRevokedPerCred-1]].Issued
	if cut > v.Before[cred] {
		v.Before[cred] = cut
	}
	for _, id := range mine {
		if v.Cookies[id].Issued <= v.Before[cred] {
			delete(v.Cookies, id) // covered by the cutoff
		}
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

// maxSignInsPerMinute bounds sign-ins with one credential, which also
// bounds how fast it can churn sign-outs (see revocations).
const maxSignInsPerMinute = 20

// allowSignIn counts a sign-in with g and reports whether it is within
// maxSignInsPerMinute.
func (a *webAuth) allowSignIn(g grant, now time.Time) bool {
	key := g.cred()
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

// checkToken compares a token (a sign-in or an API bearer token), limiting
// each client to maxFailures failed attempts a minute. The check and the
// count happen under one lock, so concurrent guesses cannot slip past the
// limit.
func (a *webAuth) checkToken(client string, now time.Time, ok func() bool) (valid, throttled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, known := a.failures[client]; !known && len(a.failures) >= maxClients {
		a.forgetFailures(now)
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

// forgetFailures makes room in failures under a flood: it drops clients
// with no failure in the last minute, then if that is not enough the tenth
// that have been quiet longest. Clients still failing keep their counts.
func (a *webAuth) forgetFailures(now time.Time) {
	last := func(ts []time.Time) time.Time {
		if len(ts) == 0 {
			return time.Time{}
		}
		return ts[len(ts)-1]
	}
	for k, ts := range a.failures {
		if now.Sub(last(ts)) >= time.Minute {
			delete(a.failures, k)
		}
	}
	if len(a.failures) < maxClients {
		return
	}
	keys := make([]string, 0, len(a.failures))
	for k := range a.failures {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return last(a.failures[keys[i]]).Before(last(a.failures[keys[j]])) })
	for _, k := range keys[:len(keys)/10+1] {
		delete(a.failures, k)
	}
}

// clientKey identifies a client for throttling: its address, or the address
// a reverse proxy on this machine forwarded for. It takes the last
// X-Forwarded-For entry, the one the proxy added; a proxy that passes the
// header through without setting it would let a client choose its own key.
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
	_, g, ok := s.sessionCookie(r)
	return g, ok
}

// sessionCookie returns the request's valid session cookie, if any, and its
// grant: __Host-swk_session, or the older swk_session.
func (s *Server) sessionCookie(r *http.Request) (string, grant, bool) {
	now := time.Now()
	for _, name := range []string{secureCookieName, cookieName} {
		c, err := r.Cookie(name)
		if err != nil {
			continue
		}
		if g, ok := s.web.validCookie(c.Value, now); ok && s.grantLive(g, now) {
			return c.Value, g, true
		}
	}
	return "", grant{}, false
}

// sessionCookieName is the cookie name for a response to r.
func sessionCookieName(r *http.Request) string {
	if secureRequest(r) {
		return secureCookieName
	}
	return cookieName
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
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName(r), Value: value, Path: "/", MaxAge: max(int(maxAge.Seconds()), 1),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	s.logf("web sign-in from %s (%s)", clientKey(r), s.grantName(g))
	writeJSON(w, 200, map[string]any{"ok": true, "scope": g.scope.String()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeErr(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return
	}
	var saveErr error
	for _, name := range []string{secureCookieName, cookieName} {
		c, err := r.Cookie(name)
		if err != nil {
			continue
		}
		if _, ok := s.web.validCookie(c.Value, time.Now()); ok {
			if err := s.web.revoke(c.Value, time.Now()); err != nil {
				saveErr = err
			}
			s.auth.recheck()
		}
	}
	for _, name := range []string{secureCookieName, cookieName} {
		if name == secureCookieName && !secureRequest(r) {
			continue // a __Host- cookie can only be set or cleared over HTTPS
		}
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	}
	if saveErr != nil {
		// The cookie is out until the coordinator restarts, but a copy of
		// it would work again after that: say so rather than report success.
		s.logf("web sign-out not saved: %v", saveErr)
		writeErr(w, http.StatusInternalServerError, errors.New("signed out, but the sign-out could not be saved; a copy of this cookie would work again after a coordinator restart"))
		return
	}
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
