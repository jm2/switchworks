// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/version"
	"github.com/jm2/switchyard/internal/wire"
)

// Handler returns the HTTPS API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"name": version.Name, "version": version.Version})
	})
	mux.HandleFunc("POST /v1/enroll", bodyDeadline(s.handleEnroll))

	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/hosts", s.handleHosts)
	authed.HandleFunc("DELETE /v1/hosts/{id}", s.handleRemoveHost)
	authed.HandleFunc("GET /v1/sessions", s.handleSessions)
	authed.HandleFunc("POST /v1/sessions", s.handleSpawn)
	authed.HandleFunc("GET /v1/sessions/{id}", s.handleSession)
	authed.HandleFunc("GET /v1/sessions/{id}/events", s.handleEvents)
	authed.HandleFunc("GET /v1/sessions/{id}/stream", s.handleStream)
	authed.HandleFunc("POST /v1/sessions/{id}/messages", s.handleMessage)
	authed.HandleFunc("POST /v1/sessions/{id}/interrupt", s.handleSimple(wire.OpInterrupt))
	authed.HandleFunc("POST /v1/sessions/{id}/stop", s.handleSimple(wire.OpStop))
	authed.HandleFunc("POST /v1/sessions/{id}/approvals/{aid}", s.handleApprove)
	authed.HandleFunc("POST /v1/enroll-tokens", s.handleEnrollToken)
	authed.HandleFunc("POST /v1/messages", s.handlePostMessage)
	authed.HandleFunc("GET /v1/messages", s.handleListMessages)
	authed.HandleFunc("GET /v1/messages/{id}", s.handleGetMessage)
	authed.HandleFunc("POST /v1/messages/{id}/reply", s.handleReplyMessage)
	authed.HandleFunc("GET /v1/usage", s.handleUsage)
	authed.HandleFunc("GET /v1/stream", s.handleFleetStream)
	authed.HandleFunc("POST /v1/ui-links", s.handleUILink)
	mux.Handle("/v1/", s.requireAuth(authed))
	s.uiRoutes(mux)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeBody(r *http.Request, v any) error { return decodeBodyLimit(r, v, 4<<20) }

// decodeBodyLimit is decodeBody with its own size limit, small for the
// endpoints anyone can reach.
func decodeBodyLimit(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ipBucket groups addresses for per-client limits: an IPv6 client usually
// has a whole /64, so it counts as one.
func ipBucket(host string) string {
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (s *Server) view(sess model.Session) apiclient.SessionView {
	v := apiclient.SessionView{Session: sess, Online: s.Online(sess.Host)}
	if !v.Online && sess.Status != model.StatusStopped && sess.Status != model.StatusGone {
		v.Status = model.StatusOffline
	}
	s.fillActivity(&v)
	return v
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts := s.store.Hosts()
	for i := range hosts {
		hosts[i].Connected = s.Online(hosts[i].ID)
	}
	writeJSON(w, 200, hosts)
}

// handleRemoveHost revokes a host: its certificate stops working, its node
// is disconnected, its enrollment tokens are dropped and its sessions are
// marked gone. Enroll it again to bring it back.
func (s *Server) handleRemoveHost(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("id")
	s.hostMu.Lock()
	err := s.store.DeleteHost(host)
	if err == nil {
		s.disconnect(host)
	}
	s.hostMu.Unlock()
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	for _, sess := range s.store.Sessions() {
		if sess.Host == host && live(sess) {
			_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) { x.Status = model.StatusGone })
			s.publishSession(sess.ID)
		}
	}
	s.fleetPublish("host_removed", map[string]string{"id": host})
	s.logf("removed host %s", host)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	out := []apiclient.SessionView{}
	for _, sess := range s.store.Sessions() {
		if path != "" && sess.Path != path {
			continue
		}
		out = append(out, s.view(sess))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, s.view(sess))
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var spec model.SpawnSpec
	if err := decodeBody(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	sess, err := s.Spawn(r.Context(), spec)
	if err == nil {
		s.router.started(sess.ID, 0) // a person's spawn starts a new chain
	}
	switch {
	case errors.Is(err, store.ErrPathInUse):
		writeErr(w, 409, fmt.Errorf("%s/%s: %w", spec.Host, spec.Name, err))
	case errors.Is(err, ErrOffline):
		writeErr(w, 409, err)
	case err != nil:
		writeErr(w, 400, err)
	default:
		writeJSON(w, 201, s.view(sess))
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Session(id); !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	evs, err := s.store.Events(id, after, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if evs == nil {
		evs = []model.Event{}
	}
	writeJSON(w, 200, evs)
}

// handleStream serves a session's events as Server-Sent Events: the stored
// backlog after ?after=N, then live events. Each event's SSE id is its seq,
// so clients resume with ?after=<last id>.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Session(id); !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if id, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > after {
		after = id // an EventSource reconnecting
	}
	// Subscribe before reading the backlog so nothing falls in between.
	ch := s.subscribe(id)
	defer s.unsubscribe(id, ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	last := after
	send := func(ev model.Event) error {
		if ev.Seq <= last {
			return nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b); err != nil {
			return err
		}
		last = ev.Seq
		return nil
	}
	// catchUp sends stored events after last, page by page (a page can be
	// short when its events are large), until none are left or upto is
	// reached.
	catchUp := func(upto int64) bool {
		for upto == 0 || last < upto {
			backlog, err := s.store.Events(id, last, 1000)
			if err != nil {
				return false
			}
			if len(backlog) == 0 {
				return true
			}
			for _, ev := range backlog {
				if upto != 0 && ev.Seq > upto {
					return true
				}
				if send(ev) != nil {
					return false
				}
			}
			flusher.Flush()
		}
		return true
	}
	if !catchUp(0) {
		return
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // dropped as a slow subscriber; client reconnects
			}
			if ev.Seq > last+1 {
				// Gap (events published before this subscriber caught up):
				// fill from the store.
				if !catchUp(ev.Seq - 1) {
					return
				}
			}
			if send(ev) != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
		From string `json:"from"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, 400, errors.New("text is required"))
		return
	}
	switch body.Mode {
	case "", model.ModeQueue, model.ModeSteer, model.ModeInterrupt:
	default:
		writeErr(w, 400, fmt.Errorf("unknown mode %q (queue, steer, interrupt)", body.Mode))
		return
	}
	from, err := userSender(body.From)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	if body.Mode == model.ModeSteer && !s.Online(sess.Host) {
		writeErr(w, 409, fmt.Errorf("host %q: %w (steering needs a live session)", sess.Host, ErrOffline))
		return
	}
	msgs, err := s.router.send(r.Context(), from, model.MsgNotify, fleet.SendArgs{To: sess.ID, Text: body.Text, Mode: body.Mode})
	if err != nil {
		writeErr(w, routeStatus(err), err)
		return
	}
	m := msgs[0]
	if m.State == model.MsgFailed {
		writeErr(w, 409, errors.New(m.Error))
		return
	}
	writeJSON(w, 202, apiclient.SendResult{MessageID: m.ID, State: m.State, Mode: m.Mode})
}

// routeStatus is the HTTP status for a routing error.
func routeStatus(err error) int {
	switch {
	case errors.Is(err, ErrRateLimit):
		return 429
	case errors.Is(err, ErrHopLimit), errors.Is(err, ErrBudget):
		return 409
	}
	return 400
}

// userSender validates a "user:<name>" sender (default user:cli).
func userSender(from string) (sender, error) {
	if from == "" {
		from = "user:cli"
	}
	if !strings.HasPrefix(from, "user:") {
		return sender{}, errors.New(`from must look like "user:<name>"`)
	}
	if err := model.ValidateName("user", strings.TrimPrefix(from, "user:")); err != nil {
		return sender{}, err
	}
	return sender{user: from}, nil
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var body apiclient.PostMessage
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	from, err := userSender(body.From)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	switch body.Kind {
	case "", model.MsgNotify, model.MsgBroadcast:
		kind := model.MsgNotify
		if body.Kind == model.MsgBroadcast {
			kind = model.MsgBroadcast
		}
		msgs, err := s.router.send(r.Context(), from, kind, fleet.SendArgs{To: body.To, Text: body.Text, Mode: body.Mode, TTLSeconds: body.TTLSeconds})
		if err != nil {
			writeErr(w, routeStatus(err), err)
			return
		}
		writeJSON(w, 202, fleet.SendResult{Messages: infos(msgs)})
	case model.MsgAsk:
		wait := 0
		if body.WaitSeconds != nil {
			wait = *body.WaitSeconds
		}
		res := s.router.ask(r.Context(), from, fleet.AskArgs{To: body.To, Text: body.Text, WaitSeconds: &wait, TTLSeconds: body.TTLSeconds})
		if res.Status == fleet.AskFailed && res.MessageID == "" {
			writeErr(w, 400, errors.New(res.Error))
			return
		}
		writeJSON(w, 202, res)
	default:
		writeErr(w, 400, fmt.Errorf("unknown kind %q (notify, ask, broadcast)", body.Kind))
	}
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	f := store.MessageFilter{Session: q.Get("session"), ToUser: q.Get("inbox") == "1", Limit: limit}
	msgs := s.store.Messages(f)
	if msgs == nil {
		msgs = []model.Message{}
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.store.Message(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleReplyMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from,omitempty"`
		Text string `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	from, err := userSender(body.From)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	res, err := s.router.reply(r.Context(), from, fleet.ReplyArgs{MessageID: r.PathValue("id"), Text: body.Text})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, res)
}

func addUsage(m map[string]model.Usage, k string, u model.Usage) {
	cur := m[k]
	cur.InputTokens += u.InputTokens
	cur.CachedInputTokens += u.CachedInputTokens
	cur.OutputTokens += u.OutputTokens
	cur.ReasoningTokens += u.ReasoningTokens
	cur.TotalTokens += u.TotalTokens
	m[k] = cur
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	rep := apiclient.UsageReport{Today: now.Format("2006-01-02"), MaxDailyTokens: s.router.guards.MaxDailyTokens,
		Daily: map[string]int64{}, ByHost: map[string]model.Usage{}, ByModel: map[string]model.Usage{}}
	for i := 0; i < 14; i++ {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		if n := s.store.DailyTokens(d); n > 0 {
			rep.Daily[d] = n
		}
	}
	rep.TodayTokens = s.store.DailyTokens(rep.Today)
	for _, sess := range s.store.Sessions() {
		if sess.Usage == nil {
			continue
		}
		addUsage(rep.ByHost, sess.Host, *sess.Usage)
		m := sess.Model
		if m == "" {
			m = "unknown"
		}
		addUsage(rep.ByModel, m, *sess.Usage)
	}
	writeJSON(w, 200, rep)
}

func (s *Server) commandError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, err)
	case errors.Is(err, ErrOffline):
		writeErr(w, 409, err)
	default:
		writeErr(w, 400, err)
	}
	return true
}

func (s *Server) handleSimple(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := s.SessionCommand(r.Context(), r.PathValue("id"), wire.Cmd{Op: op})
		if s.commandError(w, err) {
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	_, err := s.SessionCommand(r.Context(), r.PathValue("id"), wire.Cmd{Op: wire.OpResolve,
		Resolve: &wire.ResolveCmd{ApprovalID: r.PathValue("aid"), Decision: body.Decision}})
	if s.commandError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleEnrollToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host       string `json:"host"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := model.ValidateName("host", body.Host); err != nil {
		writeErr(w, 400, err)
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = time.Hour
	}
	secret := pki.RandomSecret(24)
	if err := s.store.AddToken(pki.HashSecret(secret), body.Host, time.Now().Add(ttl)); err != nil {
		writeErr(w, 500, err)
		return
	}
	tok := pki.Token{API: s.cfg.PublicAPI, Secret: secret, Fingerprint: pki.Fingerprint(s.ca.Cert), Host: body.Host}
	writeJSON(w, 201, map[string]string{"token": tok.Encode(), "expires": time.Now().Add(ttl).Format(time.RFC3339)})
}

// EnrollRequest is posted by `swyd node enroll`.
type EnrollRequest struct {
	Secret string `json:"secret"`
	CSR    string `json:"csr"`
}

// EnrollResponse returns the node's identity.
type EnrollResponse struct {
	Host     string `json:"host"`
	Cert     string `json:"cert"`
	CA       string `json:"ca"`
	NodeAddr string `json:"node_addr"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := decodeBodyLimit(r, &req, 64<<10); err != nil {
		writeErr(w, 400, err)
		return
	}
	// The token is used and the host record written under hostMu, so a
	// `hosts rm` cannot fall in between and be undone.
	s.hostMu.Lock()
	host, cert, err := s.enrollLocked(req)
	s.hostMu.Unlock()
	if err != nil {
		writeErr(w, enrollStatus(err), err)
		return
	}
	s.logf("enrolled host %s from %s", host, remoteHost(r))
	writeJSON(w, 200, EnrollResponse{Host: host, Cert: string(cert), CA: string(s.ca.PEM), NodeAddr: s.cfg.PublicNode})
}

type enrollErr struct {
	status int
	err    error
}

func (e enrollErr) Error() string { return e.err.Error() }
func (e enrollErr) Unwrap() error { return e.err }

func enrollStatus(err error) int {
	if e, ok := err.(enrollErr); ok {
		return e.status
	}
	return 500
}

// enrollLocked uses an enrollment token and signs the node's certificate,
// which replaces any earlier one for the host. Callers hold hostMu.
func (s *Server) enrollLocked(req EnrollRequest) (string, []byte, error) {
	host, err := s.store.ConsumeToken(pki.HashSecret(req.Secret))
	if err != nil {
		return "", nil, enrollErr{403, err}
	}
	cert, err := s.ca.SignNodeCSR([]byte(req.CSR), host)
	if err != nil {
		return "", nil, enrollErr{400, err}
	}
	parsed, err := pki.ParseCert(cert)
	if err != nil {
		return "", nil, err
	}
	h, known := s.store.Host(host)
	if !known {
		h = model.Host{ID: host, LastSeen: time.Now()}
	}
	h.CertSHA256 = pki.Fingerprint(parsed)
	if err := s.store.UpsertHost(h); err != nil {
		return "", nil, err
	}
	s.disconnect(host)
	return host, cert, nil
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// Connection limits for the API listener: per client address and in all.
// A client over its share is turned away at once (not queued), so one
// client holding connections open cannot lock others out, and the total
// stays well inside the coordinator's file descriptors. The plain-HTTP
// listener sees every request from its local proxy, so it has no per-client
// limit.
const (
	maxConns       = 4096
	maxConnsPerIP  = 64
	plainConnLimit = 1024
)

// httpServer configures the API server's limits. Responses have no write
// deadline, because event streams and waiting asks stay open; the
// unauthenticated endpoints get a body deadline (bodyDeadline).
func (s *Server) httpServer() *http.Server {
	return &http.Server{Handler: securityHeaders(s.Handler()), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
}

// bodyDeadline gives a request 30 seconds to send its body, for the
// endpoints anyone can reach (enrollment, sign-in), whose handlers answer
// at once.
func bodyDeadline(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		next(w, r)
	}
}

// securityHeaders adds HSTS over HTTPS. Browsers ignore it on a
// connection whose certificate they do not trust, so it cannot lock anyone
// out before the fleet CA is installed.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secureRequest(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// limitListener caps open connections, in all and (perIP > 0) per client
// address, closing new ones over a limit right away.
func limitListener(ln net.Listener, total, perIP int) net.Listener {
	return &limitedListener{Listener: ln, total: total, perIP: perIP, byIP: map[string]int{}}
}

type limitedListener struct {
	net.Listener
	total, perIP int

	mu   sync.Mutex
	open int
	byIP map[string]int
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := ""
		if h, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
			ip = ipBucket(h)
		}
		l.mu.Lock()
		if l.open >= l.total || (l.perIP > 0 && l.byIP[ip] >= l.perIP) {
			l.mu.Unlock()
			_ = c.Close()
			continue
		}
		l.open++
		l.byIP[ip]++
		l.mu.Unlock()
		return &limitedConn{Conn: c, release: func() {
			l.mu.Lock()
			l.open--
			if l.byIP[ip]--; l.byIP[ip] <= 0 {
				delete(l.byIP, ip)
			}
			l.mu.Unlock()
		}}, nil
	}
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// ServeAPI serves the HTTPS API on ln until ctx ends.
func (s *Server) ServeAPI(ctx context.Context, ln net.Listener) error {
	tlsCfg, err := pki.ServerTLS(s.dir, nil)
	if err != nil {
		return err
	}
	srv := s.httpServer()
	srv.TLSConfig = tlsCfg
	ln = limitListener(ln, maxConns, maxConnsPerIP)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err = srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ServePlain serves the same API and UI without TLS, for a reverse proxy
// on the same machine (Config.HTTPListen).
func (s *Server) ServePlain(ctx context.Context, ln net.Listener) error {
	srv := s.httpServer()
	ln = limitListener(ln, plainConnLimit, 0)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
