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
	"sync"
	"time"

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
	fleet map[chan model.Event]struct{} // subscribers to every session's events

	router *router

	nodeLn net.Listener
	wg     sync.WaitGroup
}

type nodeConn struct {
	host  string
	conn  *wire.Conn
	raw   net.Conn
	hello wire.Hello

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
		fleet: map[chan model.Event]struct{}{}}
	s.router = newRouter(s, cfg.Guards)
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
	if _, ok := s.store.Host(host); !ok {
		s.logf("rejecting node %q from %s: not enrolled", host, c.RemoteAddr())
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

	n := &nodeConn{host: host, conn: wc, raw: c, hello: hello, pending: map[string]chan cmdReply{}, closed: make(chan struct{})}
	s.mu.Lock()
	if old := s.nodes[host]; old != nil {
		go old.close()
	}
	s.nodes[host] = n
	s.mu.Unlock()
	_ = s.store.UpsertHost(model.Host{ID: host, OS: hello.OS, Arch: hello.Arch, Version: hello.Version,
		Adapters: hello.Adapters, Connected: true, LastSeen: time.Now()})
	s.logf("node %s connected from %s (%s/%s, %s, adapters %v, %d sessions)", host, c.RemoteAddr(), hello.OS, hello.Arch, hello.Version, hello.Adapters, len(hello.Sessions))

	acked := map[string]int64{}
	for _, st := range hello.Sessions {
		s.reconcile(host, st)
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
		if h, ok := s.store.Host(host); ok {
			h.Connected = false
			h.LastSeen = time.Now()
			_ = s.store.UpsertHost(h)
		}
		s.logf("node %s disconnected", host)
	}
}

// reconcile adopts sessions a node reports that the coordinator does not
// know (e.g. after coordinator data loss).
func (s *Server) reconcile(host string, st wire.SessionState) {
	if _, ok := s.store.Session(st.ID); ok {
		return
	}
	name := st.Path
	if len(name) > len(host)+1 && name[:len(host)+1] == host+"/" {
		name = name[len(host)+1:]
	}
	err := s.store.CreateSession(model.Session{ID: st.ID, Path: st.Path, Host: host, Name: name, Harness: st.Harness,
		Mode: model.ModeManaged, NativeRef: st.NativeRef, Status: st.Status, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		s.logf("node %s: adopt session %s (%s): %v", host, st.ID, st.Path, err)
	}
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
			if err := s.ingest(n.host, p.Events); err != nil {
				s.logf("node %s: store events: %v", n.host, err)
				return
			}
			acked := map[string]int64{}
			for _, ev := range p.Events {
				acked[ev.SessionID] = s.store.LastSeq(ev.SessionID)
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
			// A fleet operation from one of the node's sessions.
			go s.handleNodeReq(n, env)
		case wire.TPing:
			_ = n.conn.Send(wire.TPong, "", "", nil)
		case wire.TPong:
		default:
			s.logf("node %s: unexpected %q", n.host, env.Type)
		}
	}
}

// ingest stores events from host, projects them and notifies subscribers.
func (s *Server) ingest(host string, evs []model.Event) error {
	// Only accept events for sessions that belong to this host.
	ok := evs[:0:0]
	for _, ev := range evs {
		sess, known := s.store.Session(ev.SessionID)
		if known && sess.Host != host {
			s.logf("node %s sent event for %s's session %s; dropped", host, sess.Host, ev.SessionID)
			continue
		}
		ok = append(ok, ev)
	}
	stored, err := s.store.AppendEvents(ok)
	for _, ev := range stored {
		s.project(ev)
		s.publish(ev)
		s.route(ev)
	}
	return err
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
	for ch := range s.fleet {
		select {
		case ch <- ev:
		default:
			delete(s.fleet, ch)
			close(ch)
		}
	}
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
	n, ok := s.node(spec.Host)
	if !ok {
		if _, known := s.store.Host(spec.Host); !known {
			return model.Session{}, fmt.Errorf("unknown host %q", spec.Host)
		}
		return model.Session{}, fmt.Errorf("host %q: %w", spec.Host, ErrOffline)
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
	if _, err := s.command(ctx, spec.Host, wire.Cmd{Op: wire.OpSpawn, Session: sess.ID,
		Spawn: &wire.SpawnCmd{Session: sess, Spec: spec}}, 2*time.Minute); err != nil {
		_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) { x.Status = model.StatusError })
		return model.Session{}, fmt.Errorf("spawn on %s: %w", spec.Host, err)
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
