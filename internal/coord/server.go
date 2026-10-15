// SPDX-License-Identifier: GPL-3.0-or-later

// Package coord is the coordinator: it accepts node connections over mTLS,
// stores every session's event log, projects session state from events,
// forwards commands to nodes, and serves the HTTPS API.
package coord

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/wire"
)

// Server is a running coordinator.
type Server struct {
	cfg   Config
	dir   string
	store store.Store
	ca    *pki.CA
	logf  func(string, ...any)

	mu    sync.Mutex
	nodes map[string]*nodeConn
	subs  map[string]map[chan model.Event]struct{}
	fleet map[chan FleetEvent]struct{} // fleet stream subscribers
	acts  map[string]*activity         // session id -> recent activity

	dropLogged map[string]time.Time // host -> last "dropped input" log

	// hostMu orders changes to a host's pinned certificate (enroll, remove)
	// against nodes completing their connection.
	hostMu sync.Mutex

	router *router
	web    *webAuth

	nodeLn net.Listener
	wg     sync.WaitGroup
}

// maxNodeReqs bounds concurrent fleet operations per node.
const maxNodeReqs = 64

type nodeConn struct {
	host  string
	conn  *wire.Conn
	raw   net.Conn
	hello wire.Hello

	reqs chan struct{} // fleet operations in progress

	mu      sync.Mutex
	pending map[string]chan cmdReply
	closed  chan struct{}
	once    sync.Once
}

// cmdReply is a node's answer to a command, or the transport error that
// prevented one.
type cmdReply struct {
	r   wire.Result
	err error
}

func (n *nodeConn) close() {
	n.once.Do(func() {
		close(n.closed)
		_ = n.raw.Close()
		n.mu.Lock()
		for id, ch := range n.pending {
			ch <- cmdReply{err: fmt.Errorf("node %s disconnected: %w", n.host, ErrOffline)}
			delete(n.pending, id)
		}
		n.mu.Unlock()
	})
}

// NewServer prepares a coordinator over an opened store.
func NewServer(dir string, cfg Config, st store.Store, logf func(string, ...any)) (*Server, error) {
	ca, err := pki.LoadCA(dir)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, dir: dir, store: st, ca: ca, logf: logf,
		nodes: map[string]*nodeConn{}, subs: map[string]map[chan model.Event]struct{}{},
		fleet: map[chan FleetEvent]struct{}{}, acts: map[string]*activity{}}
	if s.web, err = loadWebAuth(dir); err != nil {
		return nil, fmt.Errorf("web sign-in key: %w", err)
	}
	s.router = newRouter(s, cfg.Guards)
	s.rebuildActivity()
	return s, nil
}

// ServeNodes accepts node connections on ln until it is closed.
func (s *Server) ServeNodes(ln net.Listener) error {
	tlsCfg, err := pki.ServerTLS(s.dir, s.ca)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.nodeLn = ln
	s.mu.Unlock()
	tl := tls.NewListener(ln, tlsCfg)
	for {
		c, err := tl.Accept()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleNode(c)
		}()
	}
}

// Shutdown closes node connections.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if s.nodeLn != nil {
		_ = s.nodeLn.Close()
	}
	nodes := make([]*nodeConn, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, n)
	}
	s.mu.Unlock()
	for _, n := range nodes {
		n.close()
	}
	s.wg.Wait()
	s.router.close()
}

func (s *Server) handleNode(c net.Conn) {
	tc := c.(*tls.Conn)
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.Handshake(); err != nil {
		s.logf("node handshake from %s: %v", c.RemoteAddr(), err)
		c.Close()
		return
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		c.Close()
		return
	}
	host := certs[0].Subject.CommonName
	fp := pki.Fingerprint(certs[0])
	if _, err := s.checkPin(host, fp); err != nil { // early refusal; checked again below
		s.logf("rejecting node %q from %s: %v", host, c.RemoteAddr(), err)
		c.Close()
		return
	}
	wc := wire.NewConn(c)
	env, err := wc.Recv()
	if err != nil || env.Type != wire.THello {
		s.logf("node %s: expected hello: %v", host, err)
		c.Close()
		return
	}
	hello, err := wire.Decode[wire.Hello](env)
	if err != nil || hello.Host != host {
		s.logf("node %s: bad hello (claims %q): %v", host, hello.Host, err)
		c.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})

	n := &nodeConn{host: host, conn: wc, raw: c, hello: hello, pending: map[string]chan cmdReply{}, closed: make(chan struct{}),
		reqs: make(chan struct{}, maxNodeReqs)}
	// The pin is checked again and the node registered under hostMu, which
	// removing or re-enrolling the host also holds: a connection that was
	// opened before either cannot complete after it.
	s.hostMu.Lock()
	rec, err := s.checkPin(host, fp)
	if err != nil {
		s.hostMu.Unlock()
		s.logf("rejecting node %q from %s: %v", host, c.RemoteAddr(), err)
		c.Close()
		return
	}
	if rec.CertSHA256 == "" {
		// Enrolled before certificates were pinned: trust the first one
		// seen. Enrolling again replaces it.
		rec.CertSHA256 = fp
		s.logf("node %s: pinned certificate %s", host, fp)
	}
	rec.OS, rec.Arch, rec.Version, rec.Adapters = hello.OS, hello.Arch, hello.Version, hello.Adapters
	rec.Connected, rec.LastSeen = true, time.Now()
	_ = s.store.UpsertHost(rec)
	s.mu.Lock()
	if old := s.nodes[host]; old != nil {
		go old.close()
	}
	s.nodes[host] = n
	s.mu.Unlock()
	s.hostMu.Unlock()
	s.logf("node %s connected from %s (%s/%s, %s, adapters %v, %d sessions)", host, c.RemoteAddr(), hello.OS, hello.Arch, hello.Version, hello.Adapters, len(hello.Sessions))
	s.publishHost(host)

	acked := map[string]int64{}
	for _, st := range hello.Sessions {
		s.reconcile(host, st)
		s.learnPolicies(host, st)
		acked[st.ID] = s.store.LastSeq(st.ID)
	}
	if err := wc.Send(wire.TWelcome, "", "", wire.Welcome{Protocol: wire.ProtocolVersion, Acked: acked}); err != nil {
		n.close()
	}

	go s.pinger(n)
	s.router.poke() // deliver messages that waited for this host
	s.readNode(n)

	s.mu.Lock()
	current := s.nodes[host] == n
	if current {
		delete(s.nodes, host)
	}
	s.mu.Unlock()
	if current {
		s.hostMu.Lock()
		if h, ok := s.store.Host(host); ok && h.CertSHA256 == fp {
			h.Connected = false
			h.LastSeen = time.Now()
			_ = s.store.UpsertHost(h)
		}
		s.hostMu.Unlock()
		s.logf("node %s disconnected", host)
		s.publishHost(host)
	}
}

// reconcile adopts sessions a node reports that the coordinator does not
// know (e.g. after coordinator data loss). The node chooses the ID and path,
// so both are checked: a canonical ULID, and a path on this host.
func (s *Server) reconcile(host string, st wire.SessionState) {
	if _, ok := s.store.Session(st.ID); ok {
		return
	}
	name, onHost := strings.CutPrefix(st.Path, host+"/")
	switch {
	case !ulid.Valid(st.ID):
		s.logf("node %s: not adopting session with invalid id %q", host, st.ID)
		return
	case !onHost || model.ValidateSessionName(name) != nil:
		s.logf("node %s: not adopting session %s with path %q", host, st.ID, st.Path)
		return
	case model.ValidateName("harness", st.Harness) != nil:
		s.logf("node %s: not adopting session %s with harness %q", host, st.ID, st.Harness)
		return
	}
	if t, err := ulid.Time(st.ID); err != nil || t.After(time.Now().Add(time.Hour)) {
		// An ID from the future would outrank every later session on its
		// path (SessionByPath returns the newest).
		s.logf("node %s: not adopting session %s: id is from the future", host, st.ID)
		return
	}
	status := st.Status
	if !knownStatus(status) {
		status = model.StatusError
	}
	err := s.store.CreateSession(model.Session{ID: st.ID, Path: st.Path, Host: host, Name: name, Harness: st.Harness,
		Mode: model.ModeManaged, NativeRef: st.NativeRef, Status: status, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		s.logf("node %s: adopt session %s (%s): %v", host, st.ID, st.Path, err)
	}
}

// learnPolicies records the approval policy and sandbox a node reports for
// one of its sessions, when the coordinator does not know them yet (sessions
// from before they were recorded, or adopted ones).
func (s *Server) learnPolicies(host string, st wire.SessionState) {
	if validateSpawn(model.SpawnSpec{Harness: "x", ApprovalPolicy: st.ApprovalPolicy, Sandbox: st.Sandbox}) != nil {
		return
	}
	_, _ = s.store.UpdateSession(st.ID, func(x *model.Session) {
		if x.Host == host && x.ApprovalPolicy == "" && x.Sandbox == "" {
			x.ApprovalPolicy, x.Sandbox = st.ApprovalPolicy, st.Sandbox
		}
	})
}

func knownStatus(st string) bool {
	switch st {
	case model.StatusStarting, model.StatusIdle, model.StatusActive, model.StatusWaitingApproval,
		model.StatusError, model.StatusStopped, model.StatusGone:
		return true
	}
	return false
}

func (s *Server) pinger(n *nodeConn) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := n.conn.Send(wire.TPing, "", "", nil); err != nil {
				n.close()
				return
			}
		case <-n.closed:
			return
		}
	}
}

func (s *Server) readNode(n *nodeConn) {
	defer n.close()
	for {
		_ = n.raw.SetReadDeadline(time.Now().Add(75 * time.Second))
		env, err := n.conn.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case wire.TEvents:
			p, err := wire.Decode[wire.Events](env)
			if err != nil {
				s.logf("node %s: bad events: %v", n.host, err)
				return
			}
			acked, err := s.ingest(n.host, p.Events)
			if err != nil {
				s.logf("node %s: store events: %v", n.host, err)
				return
			}
			if err := n.conn.Send(wire.TAck, "", "", wire.Ack{Acked: acked}); err != nil {
				return
			}
		case wire.TResult:
			r, _ := wire.Decode[wire.Result](env)
			n.mu.Lock()
			ch := n.pending[env.Re]
			delete(n.pending, env.Re)
			n.mu.Unlock()
			if ch != nil {
				ch <- cmdReply{r: r}
			}
		case wire.TCmd:
			// A fleet operation from one of the node's sessions, at most
			// maxNodeReqs at a time per node (asks can wait for minutes).
			select {
			case n.reqs <- struct{}{}:
				go func() {
					defer func() { <-n.reqs }()
					s.handleNodeReq(n, env)
				}()
			default:
				_ = n.conn.Send(wire.TResult, "", env.ID, wire.Result{Error: "coordinator busy: too many fleet operations from this host; try again shortly"})
			}
		case wire.TPing:
			_ = n.conn.Send(wire.TPong, "", "", nil)
		case wire.TPong:
		default:
			s.logf("node %s: unexpected %q", n.host, env.Type)
		}
	}
}

// ingest stores events from host, projects them and notifies subscribers.
// It returns the highest seq to acknowledge per session: events it refuses
// are acknowledged too, so the node drops them from its spool rather than
// resending them forever.
func (s *Server) ingest(host string, evs []model.Event) (map[string]int64, error) {
	acked := map[string]int64{}
	ok := evs[:0:0]
	for _, ev := range evs {
		// Only sessions this coordinator created (or adopted from this
		// host's hello) and that belong to this host: a node must not write
		// into another host's logs or invent sessions.
		sess, known := s.store.Session(ev.SessionID)
		if !known || sess.Host != host || !ulid.Valid(ev.SessionID) {
			s.logDrop(host, "event for session %q it does not own", ev.SessionID)
			if known && sess.Host == host && ev.Seq > acked[ev.SessionID] {
				acked[ev.SessionID] = ev.Seq
			}
			continue
		}
		if !validKind(ev.Kind) {
			s.logDrop(host, "event with kind %q", ev.Kind)
			continue
		}
		ok = append(ok, ev)
	}
	stored, err := s.store.AppendEvents(ok)
	for _, ev := range stored {
		s.project(ev)
		s.publish(ev)
		if s.track(ev) {
			s.publishSession(ev.SessionID)
		}
		s.route(ev)
	}
	for _, ev := range ok {
		acked[ev.SessionID] = s.store.LastSeq(ev.SessionID)
	}
	return acked, err
}

// validKind accepts event kinds shaped like the ones nodes emit
// ("item.delta", "agy.step"): they reach SSE "event:" lines and the UI.
func validKind(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// logDrop logs refused input from a node, at most once a minute per host.
func (s *Server) logDrop(host, format string, args ...any) {
	s.mu.Lock()
	if s.dropLogged == nil {
		s.dropLogged = map[string]time.Time{}
	}
	last := s.dropLogged[host]
	now := time.Now()
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.dropLogged[host] = now
	s.mu.Unlock()
	s.logf("node %s: dropped "+format, append([]any{host}, args...)...)
}

// route feeds events the message router reacts to.
func (s *Server) route(ev model.Event) {
	switch ev.Kind {
	case model.KindMessageDelivered:
		var p model.MessageEvent
		if json.Unmarshal(ev.Payload, &p) == nil {
			s.router.onDelivered(ev.SessionID, p.MessageID)
		}
	case model.KindSessionSettled:
		var p model.Settled
		if json.Unmarshal(ev.Payload, &p) == nil {
			if sess, ok := s.store.Session(ev.SessionID); ok {
				s.router.onSettled(sess, p)
			}
		}
	}
}

// project updates the session record from one event, and the fleet's
// daily token total from usage events.
func (s *Server) project(ev model.Event) {
	var tokenDelta int64
	defer func() {
		if tokenDelta > 0 {
			s.store.AddDailyTokens(ev.TS.UTC().Format("2006-01-02"), tokenDelta)
		}
	}()
	_, _ = s.store.UpdateSession(ev.SessionID, func(sess *model.Session) {
		sess.UpdatedAt = ev.TS
		switch ev.Kind {
		case model.KindSessionRegistered:
			var p model.SessionRegistered
			if json.Unmarshal(ev.Payload, &p) == nil {
				ref := p.NativeRef
				sess.NativeRef = &ref
				if p.Cwd != "" {
					sess.Cwd = p.Cwd
				}
				if p.Model != "" {
					sess.Model = p.Model
				}
				if p.Provider != "" {
					sess.Provider = p.Provider
				}
				sess.Caps = p.Caps
			}
		case model.KindSessionStatus:
			var p model.SessionStatus
			if json.Unmarshal(ev.Payload, &p) == nil {
				sess.Status = p.Status
			}
		case model.KindUsage:
			var p model.UsageEvent
			if json.Unmarshal(ev.Payload, &p) == nil {
				u := p.Total
				tokenDelta = u.TotalTokens
				if sess.Usage != nil && sess.Usage.TotalTokens <= u.TotalTokens {
					tokenDelta = u.TotalTokens - sess.Usage.TotalTokens
				}
				sess.Usage = &u
				if p.Model != "" {
					sess.Model = p.Model
				}
			}
		}
	})
}

func (s *Server) subscribe(sessionID string) chan model.Event {
	ch := make(chan model.Event, 1024)
	s.mu.Lock()
	m := s.subs[sessionID]
	if m == nil {
		m = map[chan model.Event]struct{}{}
		s.subs[sessionID] = m
	}
	m[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(sessionID string, ch chan model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.subs[sessionID]; m != nil {
		if _, ok := m[ch]; ok {
			delete(m, ch)
			close(ch)
		}
		if len(m) == 0 {
			delete(s.subs, sessionID)
		}
	}
}

func (s *Server) publish(ev model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs[ev.SessionID] {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop it; the client reconnects from its
			// last seq and reads the backlog from the store.
			delete(s.subs[ev.SessionID], ch)
			close(ch)
		}
	}
}

// checkPin returns host's record if a node with certificate fingerprint fp
// may connect: the host is enrolled and fp is its pinned certificate (or
// none is pinned yet).
func (s *Server) checkPin(host, fp string) (model.Host, error) {
	rec, ok := s.store.Host(host)
	switch {
	case !ok:
		return rec, errors.New("not enrolled")
	case rec.CertSHA256 != "" && rec.CertSHA256 != fp:
		return rec, fmt.Errorf("certificate %s was replaced or revoked", fp)
	}
	return rec, nil
}

// disconnect drops a host's node connection, if any.
func (s *Server) disconnect(host string) {
	if n, ok := s.node(host); ok {
		n.close()
	}
}

// ErrOffline means the target host is not connected.
var ErrOffline = errors.New("host is not connected")

func (s *Server) node(host string) (*nodeConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[host]
	return n, ok
}

// command sends a command to a host's node and waits for its result.
func (s *Server) command(ctx context.Context, host string, cmd wire.Cmd, timeout time.Duration) (wire.Result, error) {
	n, ok := s.node(host)
	if !ok {
		return wire.Result{}, ErrOffline
	}
	id := ulid.New()
	ch := make(chan cmdReply, 1)
	n.mu.Lock()
	n.pending[id] = ch
	n.mu.Unlock()
	if err := n.conn.Send(wire.TCmd, id, "", cmd); err != nil {
		n.mu.Lock()
		delete(n.pending, id)
		n.mu.Unlock()
		return wire.Result{}, fmt.Errorf("send to %s: %v: %w", host, err, ErrOffline)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case rep := <-ch:
		if rep.err != nil {
			return wire.Result{}, rep.err
		}
		if !rep.r.OK {
			return rep.r, &errRejected{msg: rep.r.Error}
		}
		return rep.r, nil
	case <-ctx.Done():
		n.mu.Lock()
		delete(n.pending, id)
		n.mu.Unlock()
		return wire.Result{}, fmt.Errorf("node %s did not answer: %w", host, ctx.Err())
	}
}

// Online reports whether a host is connected.
func (s *Server) Online(host string) bool {
	_, ok := s.node(host)
	return ok
}

// Spawn validates a request, records the session and asks its node to start it.
func (s *Server) Spawn(ctx context.Context, spec model.SpawnSpec) (model.Session, error) {
	if spec.Harness == "" {
		spec.Harness = "codex"
	}
	if err := model.ValidateName("host", spec.Host); err != nil {
		return model.Session{}, err
	}
	if err := model.ValidateSessionName(spec.Name); err != nil {
		return model.Session{}, err
	}
	if spec.Cwd == "" {
		return model.Session{}, errors.New("cwd is required (a directory on the target host)")
	}
	if err := validateSpawn(spec); err != nil {
		return model.Session{}, err
	}
	n, ok := s.node(spec.Host)
	if !ok {
		if _, known := s.store.Host(spec.Host); !known {
			return model.Session{}, fmt.Errorf("unknown host %q", spec.Host)
		}
		return model.Session{}, fmt.Errorf("host %q: %w", spec.Host, ErrOffline)
	}
	if spec.CwdRoot != "" && !contains(n.hello.Features, wire.FeatureCwdRoot) {
		return model.Session{}, fmt.Errorf("host %q runs an older swyd that cannot confine sessions spawned by agents; upgrade it", spec.Host)
	}
	if !contains(n.hello.Adapters, spec.Harness) {
		return model.Session{}, fmt.Errorf("host %q has no %s harness (available: %v)", spec.Host, spec.Harness, n.hello.Adapters)
	}
	now := time.Now()
	sess := model.Session{
		ID: ulid.New(), Path: model.JoinPath(spec.Host, spec.Name), Host: spec.Host, Name: spec.Name,
		Harness: spec.Harness, Mode: model.ModeManaged, Cwd: spec.Cwd, Labels: spec.Labels,
		Status: model.StatusStarting, Parent: spec.Parent, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateSession(sess); err != nil {
		return model.Session{}, err
	}
	s.publishSession(sess.ID)
	res, err := s.command(ctx, spec.Host, wire.Cmd{Op: wire.OpSpawn, Session: sess.ID,
		Spawn: &wire.SpawnCmd{Session: sess, Spec: spec}}, 2*time.Minute)
	if err != nil {
		_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) { x.Status = model.StatusError })
		s.publishSession(sess.ID)
		return model.Session{}, fmt.Errorf("spawn on %s: %w", spec.Host, err)
	}
	var sr wire.SpawnResult
	if json.Unmarshal(res.Data, &sr) == nil {
		_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) {
			x.ApprovalPolicy, x.Sandbox = sr.ApprovalPolicy, sr.Sandbox
		})
	}
	out, _ := s.store.Session(sess.ID)
	return out, nil
}

// SessionCommand forwards an operation on an existing session.
func (s *Server) SessionCommand(ctx context.Context, id string, cmd wire.Cmd) (wire.Result, error) {
	sess, ok := s.store.Session(id)
	if !ok {
		return wire.Result{}, store.ErrNotFound
	}
	cmd.Session = id
	return s.command(ctx, sess.Host, cmd, 30*time.Second)
}

var (
	modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
	labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// validateSpawn checks the fields of a spawn that reach a harness's command
// line or the fleet's routing.
func validateSpawn(spec model.SpawnSpec) error {
	if err := model.ValidateName("harness", spec.Harness); err != nil {
		return err
	}
	for what, v := range map[string]string{"model": spec.Model, "provider": spec.Provider} {
		if v != "" && !modelRe.MatchString(v) {
			return fmt.Errorf("invalid %s %q", what, v)
		}
	}
	switch spec.ApprovalPolicy {
	case "", "never", "on-request", "on-failure", "untrusted":
	default:
		return fmt.Errorf("unknown approval policy %q (never, on-request, on-failure, untrusted)", spec.ApprovalPolicy)
	}
	switch spec.Sandbox {
	case "", "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("unknown sandbox %q (read-only, workspace-write, danger-full-access)", spec.Sandbox)
	}
	if len(spec.Prompt) > fleet.MaxTextBytes {
		return fmt.Errorf("prompt is %d bytes; the limit is %d", len(spec.Prompt), fleet.MaxTextBytes)
	}
	if len(spec.Cwd) > 4096 || strings.ContainsAny(spec.Cwd, "\x00\n\r") {
		return errors.New("invalid cwd")
	}
	if len(spec.Labels) > 16 {
		return errors.New("at most 16 labels")
	}
	for k, v := range spec.Labels {
		if !labelRe.MatchString(k) {
			return fmt.Errorf("invalid label key %q", k)
		}
		if len(v) > 128 || strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return fmt.Errorf("invalid value for label %q", k)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
