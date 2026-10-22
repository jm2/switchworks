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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// Server is a running coordinator.
type Server struct {
	cfg   Config
	dir   string
	store store.Store
	ca    *pki.CA
	logf  func(string, ...any)

	// policyLocks orders approval policy changes per session.
	policyLocks keyedLocks

	mu    sync.Mutex
	nodes map[string]*nodeConn
	subs  map[string]map[chan model.Event]struct{}
	fleet map[chan FleetEvent]struct{} // fleet stream subscribers
	acts  map[string]*activity         // session id -> recent activity

	dropLogged map[string]time.Time // host -> last "dropped input" log
	onceLogged map[string]bool      // refusals already logged (logOnce)

	// ingestMu, per host, keeps an old connection's last batch and the new
	// connection's resend from being stored and projected out of order.
	ingestMu map[string]*sync.Mutex

	// hostMu orders changes to a host's pinned certificate (enroll, remove)
	// against nodes completing their connection.
	hostMu sync.Mutex

	router *router
	web    *webAuth
	auth   authWatch  // ends open requests whose credential was revoked
	blobs  *blobStore // image attachments (blobs.go)
	usage  usageBook  // adapters' last usage totals (usage.go)
	// pendingLogs: event logs of deleted sessions not yet removed, as
	// last reported (pruneSessions, on the router's goroutine).
	pendingLogs int
	// diskLow: the data volume is nearly full (watchDisk).
	diskLow atomic.Bool

	nodeLn net.Listener
	wg     sync.WaitGroup
}

// maxNodeReqs bounds concurrent fleet operations per node.
// Each session on a node has at most 8 in progress (the node's
// maxSessionReqs), so this is room for many sessions' waiting asks.
const maxNodeReqs = 256

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

	// welcomed is closed once the welcome is sent. The node is registered
	// (and can be sent commands) before that, and expects the welcome
	// first: a command ahead of it would end the connection.
	welcomed chan struct{}
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
	s.web.adminGen = adminGeneration(cfg.AdminTokenHash)
	if s.web.notice != "" {
		s.logf("WARNING: %s", s.web.notice)
	}
	if s.blobs, err = openBlobStore(filepath.Join(dir, "attachments")); err != nil {
		return nil, fmt.Errorf("attachment store: %w", err)
	}
	s.router = newRouter(s, cfg.Guards)
	s.replayProjections()
	s.purgeFailedSpawns()
	s.router.failOrphanedAsks()
	s.rebuildActivity()
	// Hosts enrolled before certificates were pinned still pin the first
	// valid certificate for their name (ServeNodes); say which, so they can
	// be re-enrolled to close that window.
	for _, h := range st.Hosts() {
		if h.CertSHA256 == "" {
			s.logf("WARNING: host %s has no pinned certificate; the first node certificate the fleet CA signed for that name to connect will be pinned. Re-enroll it (swk coordinator enroll-token %s, then swk node enroll - on the host) to pin its certificate now", h.ID, h.ID)
		}
	}
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
	// Connections are capped before the handshake, as on the API listener,
	// so unauthenticated clients cannot use up file descriptors.
	tl := tls.NewListener(limitListener(ln, maxConns, maxConnsPerIP), tlsCfg)
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
		reqs: make(chan struct{}, maxNodeReqs), welcomed: make(chan struct{})}
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
	rec.Images = hello.Images
	rec.Files = contains(hello.Features, wire.FeatureFiles)
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

	acked := s.greet(host, hello)
	if err := wc.Send(wire.TWelcome, "", "", wire.Welcome{Protocol: wire.ProtocolVersion, Acked: acked}); err != nil {
		n.close()
	}
	close(n.welcomed)

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

// greet takes in a node's hello: its host's sessions it no longer has are
// gone, those it has and the coordinator does not are adopted. It returns
// the welcome's acks: each reported session's last stored seq, or, for one
// that is not this host's here, everything the node has, so the node drops
// events it would otherwise resend forever.
func (s *Server) greet(host string, hello wire.Hello) map[string]int64 {
	defer s.lockIngest(host)()
	if len(hello.Sessions) > maxHelloSessions {
		s.logf("node %s reports %d sessions; considering the first %d", host, len(hello.Sessions), maxHelloSessions)
		hello.Sessions = hello.Sessions[:maxHelloSessions]
	} else {
		// Before adopting, so a session the node reports can take the path
		// of one it does not.
		s.markUnreported(host, hello.Sessions, time.Now())
	}
	acked := map[string]int64{}
	for _, st := range hello.Sessions {
		s.reconcile(host, st)
		s.learnPolicies(host, st)
		if sess, ok := s.store.Session(st.ID); ok && sess.Host == host {
			acked[st.ID] = s.store.LastSeq(st.ID)
		} else {
			acked[st.ID] = st.LastSeq
		}
	}
	return acked
}

// unreportedGrace is how old a session must be before a hello that does
// not list it marks it gone: a spawn sent on an earlier connection may not
// have reached the node when it said hello.
const unreportedGrace = 5 * time.Minute

// markUnreported marks gone the live sessions of host that its node no
// longer has (it lists every session it has not stopped). A coordinator
// that crashed mid-spawn, or lost their last status, would otherwise keep
// them, and their paths, forever.
func (s *Server) markUnreported(host string, reported []wire.SessionState, now time.Time) {
	has := map[string]bool{}
	for _, st := range reported {
		has[st.ID] = true
	}
	for _, sess := range s.store.Sessions() {
		if sess.Host != host || !live(sess) || has[sess.ID] || now.Sub(sess.CreatedAt) < unreportedGrace {
			continue
		}
		cur, err := s.store.UpdateSession(sess.ID, func(x *model.Session) {
			if live(*x) {
				x.Status = model.StatusGone
			}
		})
		if err != nil || cur.Status != model.StatusGone {
			continue
		}
		s.logf("node %s no longer has %s (%s, %s): marked gone", host, sess.Path, sess.ID, sess.Status)
		s.publishSession(sess.ID)
		s.router.failAsksTo(cur, "its session is gone")
	}
}

// reconcile adopts sessions a node reports that the coordinator does not
// know (e.g. after coordinator data loss). The node chooses the ID and path,
// so both are checked: a canonical ULID, and a path on this host.
func (s *Server) reconcile(host string, st wire.SessionState) {
	if _, ok := s.store.Session(st.ID); ok {
		return
	}
	if s.store.Retired(st.ID) {
		// Removed for good (swk rm, or past the retention): its node may
		// still hold it, but neither it nor its id comes back.
		s.logOnce(st.ID, "node %s: not adopting session %s (%s): it was removed; its events are acknowledged and dropped", host, st.ID, st.Path)
		return
	}
	name, onHost := strings.CutPrefix(st.Path, host+"/")
	switch {
	case !ulid.Valid(st.ID):
		s.logOnce(st.ID, "node %s: not adopting session with invalid id %q", host, st.ID)
		return
	case !onHost || model.ValidateSessionName(name) != nil:
		s.logOnce(st.ID, "node %s: not adopting session %s with path %q", host, st.ID, st.Path)
		return
	case model.ValidateName("harness", st.Harness) != nil:
		s.logOnce(st.ID, "node %s: not adopting session %s with harness %q", host, st.ID, st.Harness)
		return
	}
	if t, err := ulid.Time(st.ID); err != nil || t.After(time.Now().Add(time.Hour)) {
		// An ID from the future would outrank every later session on its
		// path (SessionByPath returns the newest).
		s.logOnce(st.ID, "node %s: not adopting session %s: id is from the future", host, st.ID)
		return
	}
	status := st.Status
	if !knownStatus(status) {
		status = model.StatusError
	}
	// Events already in its log (a coordinator that lost state.json but
	// not events/) are history: the hello says how it is now.
	last := s.store.LastSeq(st.ID)
	err := s.store.CreateSession(model.Session{ID: st.ID, Path: st.Path, Host: host, Name: name, Harness: st.Harness,
		Mode: model.ModeManaged, NativeRef: st.NativeRef, Status: status, LastSeq: last, ProjectedSeq: last,
		CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		s.logOnce(st.ID, "node %s: not adopting session %s (%s): %v; its events are acknowledged and dropped", host, st.ID, st.Path, err)
	}
}

// logOnce logs a refusal the first time it happens for key; nodes repeat
// what is refused on every connection.
func (s *Server) logOnce(key, format string, args ...any) {
	s.mu.Lock()
	if s.onceLogged == nil || len(s.onceLogged) > 10000 {
		s.onceLogged = map[string]bool{}
	}
	seen := s.onceLogged[key]
	s.onceLogged[key] = true
	s.mu.Unlock()
	if !seen {
		s.logf(format, args...)
	}
}

// lockIngest serializes storing and projecting a host's events (and its
// hellos), and returns the unlock.
func (s *Server) lockIngest(host string) func() {
	s.mu.Lock()
	if s.ingestMu == nil {
		s.ingestMu = map[string]*sync.Mutex{}
	}
	m := s.ingestMu[host]
	if m == nil {
		m = &sync.Mutex{}
		s.ingestMu[host] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
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
			x.ApprovalPolicy, x.Sandbox, x.PoliciesReported = st.ApprovalPolicy, st.Sandbox, true
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
	seen := time.Now() // connecting set the host's LastSeen
	for {
		_ = n.raw.SetReadDeadline(time.Now().Add(75 * time.Second))
		env, err := n.conn.Recv()
		if err != nil {
			return
		}
		if now := time.Now(); now.Sub(seen) >= lastSeenEvery {
			seen = now
			s.touchHost(n, now)
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
				// Counted in s.wg (this connection's own count is held
				// meanwhile), so Shutdown waits for it before the store
				// is closed.
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
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

// lastSeenEvery is how often a connected host's LastSeen is brought up to
// date from its traffic (at least the pongs to pinger's pings).
const lastSeenEvery = time.Minute

// touchHost records that n, if still its host's connection, was heard from.
func (s *Server) touchHost(n *nodeConn, now time.Time) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if cur, ok := s.node(n.host); !ok || cur != n {
		return
	}
	if h, ok := s.store.Host(n.host); ok {
		h.LastSeen = now
		_ = s.store.UpsertHost(h)
	}
}

// maxHelloSessions bounds the sessions one hello can report (and adopt).
const maxHelloSessions = 1000

// maxTokensPerEvent bounds the usage one event can add to the daily total.
const maxTokensPerEvent = 20_000_000

// ingest stores events from host, projects them and notifies subscribers.
// It returns the highest seq to acknowledge per session: events it refuses
// are acknowledged too, so the node drops them from its spool rather than
// resending them forever.
func (s *Server) ingest(host string, evs []model.Event) (map[string]int64, error) {
	// One batch of a host's at a time, stored and projected in order, even
	// while a replaced connection finishes its last one.
	defer s.lockIngest(host)()
	acked := map[string]int64{}
	refused := map[string]int64{} // session -> highest refused seq
	ok := evs[:0:0]
	for _, ev := range evs {
		// Only sessions this coordinator created (or adopted from this
		// host's hello) and that belong to this host: a node must not write
		// into another host's logs or invent sessions. Acknowledging what
		// is refused only drops it from this node's own spool.
		sess, known := s.store.Session(ev.SessionID)
		if !known || sess.Host != host || !ulid.Valid(ev.SessionID) {
			s.logDrop(host, "event for session %q it does not own", ev.SessionID)
			refused[ev.SessionID] = max(refused[ev.SessionID], ev.Seq)
			continue
		}
		if !validKind(ev.Kind) {
			s.logDrop(host, "event with kind %q", ev.Kind)
			refused[ev.SessionID] = max(refused[ev.SessionID], ev.Seq)
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
	for id, seq := range refused {
		// Past a stored event only once all of them were stored: an ack
		// above one that failed would drop it from the spool.
		if _, mixed := acked[id]; !mixed || err == nil {
			acked[id] = max(acked[id], seq)
		}
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
			s.router.onDelivered(ev.SessionID, p.MessageID, p.Mode)
			s.router.entered(ev.SessionID, p.MessageID, ev.TS)
		}
	case model.KindSessionStatus:
		// An idle session's turn is over: the next one counts hops anew.
		var p model.SessionStatus
		if json.Unmarshal(ev.Payload, &p) == nil && p.Status == model.StatusIdle {
			s.router.turnEnded(ev.SessionID)
		}
	case model.KindMessageSeen:
		// The turn that carried it has completed.
		var p model.MessageSeen
		if json.Unmarshal(ev.Payload, &p) == nil {
			s.router.onSeen(ev.SessionID, p, ev.TS)
		}
	case model.KindSessionSettled:
		var p model.Settled
		if json.Unmarshal(ev.Payload, &p) == nil {
			if sess, ok := s.store.Session(ev.SessionID); ok {
				s.router.onSettled(sess, p)
			}
		}
	case model.KindMessageFailed:
		// The node accepted the message but its harness did not take it.
		var p model.MessageEvent
		if json.Unmarshal(ev.Payload, &p) == nil {
			s.router.onFailed(ev.SessionID, p.MessageID, p.Error)
		}
	case model.KindMessageExpired:
		// The node dropped a queued message past its expiry.
		var p model.MessageEvent
		if json.Unmarshal(ev.Payload, &p) == nil {
			s.router.onExpired(ev.SessionID, p.MessageID)
		}
	}
}

// project updates the session record from one event, and the fleet's
// daily token total from usage events. An event at or below the record's
// ProjectedSeq is already in it.
func (s *Server) project(ev model.Event) {
	var tokenDelta int64
	var before model.Session
	applied := false
	defer func() {
		// Booked by the coordinator's clock (the one the budget is checked
		// against), and bounded per event, so a node's clock or report
		// cannot move usage out of today or blow the budget for everyone.
		if tokenDelta > maxTokensPerEvent {
			tokenDelta = maxTokensPerEvent
		}
		if tokenDelta > 0 {
			s.store.AddDailyTokens(time.Now().UTC().Format("2006-01-02"), tokenDelta)
		}
		if applied {
			s.projected(before, ev.SessionID)
		}
	}()
	var usage model.UsageEvent
	var added *model.Usage
	if ev.Kind == model.KindUsage && json.Unmarshal(ev.Payload, &usage) == nil {
		prev, known := s.usage.prev(s.store, ev.SessionID, ev.Seq)
		a := usageAdded(prev, known, usage)
		added = &a
		s.usage.record(ev.SessionID, usage.Total)
	}
	_, _ = s.store.UpdateSession(ev.SessionID, func(sess *model.Session) {
		if ev.Seq <= sess.ProjectedSeq {
			return
		}
		before, applied = *sess, true
		sess.ProjectedSeq = ev.Seq
		sess.UpdatedAt = ev.TS
		switch ev.Kind {
		case model.KindSessionRegistered:
			var p model.SessionRegistered
			if json.Unmarshal(ev.Payload, &p) == nil {
				ref := p.NativeRef
				sess.NativeRef = &ref
				// The node's word on the cwd counts only where the
				// coordinator has none (adopted sessions): agent spawns
				// are confined relative to it.
				if p.Cwd != "" && sess.Cwd == "" {
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
			// A status the coordinator does not know (a newer or broken
			// node) is kept as error rather than stored as given: it
			// reaches the UI, and live() and the selectors read it.
			if json.Unmarshal(ev.Payload, &p) == nil {
				sess.Status = p.Status
				if !knownStatus(p.Status) {
					sess.Status = model.StatusError
				}
			}
		case model.KindSessionQueue:
			var p model.SessionQueue
			if json.Unmarshal(ev.Payload, &p) == nil && p.Queued >= 0 {
				sess.Queued = p.Queued
			}
		case model.KindSessionPolicy:
			// handlePolicy records the value it sent; this event keeps a
			// change the record missed (a coordinator that stopped before
			// saving it). It is the node's word, so like the policies a node
			// reports at spawn it counts only on that node's host.
			var p model.SessionPolicy
			if json.Unmarshal(ev.Payload, &p) == nil && p.ApprovalPolicy != sess.ApprovalPolicy {
				switch p.ApprovalPolicy {
				case "untrusted", "on-request", "never":
					sess.ApprovalPolicy, sess.PoliciesReported = p.ApprovalPolicy, true
				}
			}
		case model.KindUsage:
			// The session's usage is the sum of what each event added
			// (usage.go), so it survives the adapter's Total starting over.
			if added != nil {
				var u model.Usage
				if sess.Usage != nil {
					u = *sess.Usage
				}
				addUsageTo(&u, *added)
				sess.Usage = &u
				tokenDelta = added.TotalTokens
				if usage.Model != "" {
					sess.Model = usage.Model
				}
			}
		}
	})
}

// projected acts on a session's change of state by an event: one that
// ended fails the asks it will not answer, and one that came back on a
// path another live session has taken since is refused.
func (s *Server) projected(before model.Session, id string) {
	after, ok := s.store.Session(id)
	if !ok {
		return
	}
	if live(before) && !live(after) {
		s.router.failAsksTo(after, "its session "+after.Status)
	}
	if store.Reusable(before) && !store.Reusable(after) {
		s.refuseRevival(after)
	}
}

// refuseRevival marks gone a session that came back to life (a spawn that
// timed out and started after all, or a session of a removed host enrolled
// again) when another live session took its path meanwhile, and stops it:
// one path, one live session.
func (s *Server) refuseRevival(sess model.Session) {
	for _, o := range s.store.Sessions() {
		if o.ID == sess.ID || o.Path != sess.Path || store.Reusable(o) {
			continue
		}
		cur, err := s.store.UpdateSession(sess.ID, func(x *model.Session) { x.Status = model.StatusGone })
		if err != nil {
			return
		}
		s.logf("%s (%s) came back after %s took its path: marked gone, stopping it", sess.Path, sess.ID, o.ID)
		s.publishSession(sess.ID)
		s.router.failAsksTo(cur, "its session is gone")
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := s.SessionCommand(ctx, sess.ID, wire.Cmd{Op: wire.OpStop}); err != nil {
				s.logf("stop %s (%s): %v", sess.Path, sess.ID, err)
			}
		}()
		return
	}
}

// replayProjections projects again the events a crash kept out of their
// sessions' records: events are durable before they are acknowledged, the
// records only at the next snapshot, and the node does not send acked
// events again.
func (s *Server) replayProjections() {
	for _, sess := range s.store.Sessions() {
		after, n := sess.ProjectedSeq, 0
		for after < sess.LastSeq {
			evs, err := s.store.Events(sess.ID, after, 400)
			if err != nil || len(evs) == 0 {
				break
			}
			for _, ev := range evs {
				s.project(ev)
			}
			n += len(evs)
			after = evs[len(evs)-1].Seq
		}
		if n > 0 {
			s.logf("%s (%s): projected %d events again", sess.Path, sess.ID, n)
		}
	}
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-n.welcomed:
	case <-n.closed:
		return wire.Result{}, ErrOffline
	case <-ctx.Done():
		return wire.Result{}, fmt.Errorf("%s: %w", host, ctx.Err())
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
	if (spec.CwdRoot != "" || spec.ByAgent) &&
		(!contains(n.hello.Features, wire.FeatureCwdRoot) || !contains(n.hello.Features, wire.FeatureAgentSpawn)) {
		return model.Session{}, fmt.Errorf("host %q runs an older swk that cannot confine sessions spawned by agents; upgrade it", spec.Host)
	}
	if !contains(n.hello.Adapters, spec.Harness) {
		return model.Session{}, fmt.Errorf("host %q has no %s harness (available: %v)", spec.Host, spec.Harness, n.hello.Adapters)
	}
	if len(spec.Attachments) > 0 {
		if strings.TrimSpace(spec.Prompt) == "" {
			return model.Session{}, errors.New("images need a prompt to go with them")
		}
		if err := s.imagesRefusal(spec.Host, spec.Harness, nil); err != nil {
			return model.Session{}, err
		}
		// The node gets the bytes with the spawn; the store keeps them for
		// the web UI.
		atts, err := s.storeAttachments(spec.Attachments)
		if err != nil {
			return model.Session{}, err
		}
		spec.Attachments = atts
	}
	now := time.Now()
	sess := model.Session{
		ID: ulid.New(), Path: model.JoinPath(spec.Host, spec.Name), Host: spec.Host, Name: spec.Name,
		Harness: spec.Harness, Mode: model.ModeManaged, Cwd: spec.Cwd, Labels: spec.Labels,
		Status: model.StatusStarting, Parent: spec.Parent, AcceptSteer: spec.AcceptSteer, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateSession(sess); err != nil {
		return model.Session{}, err
	}
	s.publishSession(sess.ID)
	sc := &wire.SpawnCmd{Session: sess, Spec: spec}
	// A person's first prompt carries their credential, like their
	// messages; an agent's arrives as its own routed message.
	sc.PromptVia, sc.PromptOwner, _ = s.credential(ctx)
	res, err := s.command(ctx, spec.Host, wire.Cmd{Op: wire.OpSpawn, Session: sess.ID, Spawn: sc}, 2*time.Minute)
	if err != nil {
		if rej := (*errRejected)(nil); errors.As(err, &rej) {
			// The node refused (e.g. no such cwd), so nothing ran: forget
			// the session rather than leave a dead row on its path.
			s.forgetSession(sess.ID)
		} else {
			// No answer (timeout, disconnect): the node may have started
			// it, and its events will bring the record up to date.
			_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) { x.Status = model.StatusError })
			s.publishSession(sess.ID)
		}
		return model.Session{}, fmt.Errorf("spawn on %s: %w", spec.Host, err)
	}
	var sr wire.SpawnResult
	if json.Unmarshal(res.Data, &sr) == nil {
		// What the coordinator asked for stands; the node's answer only
		// fills in its defaults, and is marked as reported.
		_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) {
			// Except a cwd under "~", which only the node can expand, and
			// an agent's spawn: the node resolved its symlinks, and that
			// real path, not text a symlink could later point elsewhere,
			// is what the child's own children are confined to.
			if sr.Cwd != "" && (strings.HasPrefix(spec.Cwd, "~") || spec.ByAgent) {
				x.Cwd = sr.Cwd
			}
			if spec.ApprovalPolicy == "" {
				x.ApprovalPolicy, x.PoliciesReported = sr.ApprovalPolicy, true
			} else {
				x.ApprovalPolicy = spec.ApprovalPolicy
			}
			if spec.Sandbox == "" {
				x.Sandbox, x.PoliciesReported = sr.Sandbox, true
			} else {
				x.Sandbox = spec.Sandbox
			}
		})
	}
	s.publishSession(sess.ID)
	out, _ := s.store.Session(sess.ID)
	return out, nil
}

// forgetSession deletes a session that never ran, with its events and
// activity, and tells the fleet views to drop it.
func (s *Server) forgetSession(id string) {
	if err := s.store.DeleteSession(id); err != nil {
		if _, ok := s.store.Session(id); ok {
			s.logf("forget session %s: %v", id, err)
			return
		}
		// Deleted; only its event log could not be removed yet, and the
		// store keeps trying.
		s.logf("forget session %s: %v", id, err)
	}
	s.forgotten(id)
}

// forgotten drops what the coordinator holds for a session the store no
// longer has, and tells the fleet views to drop it.
func (s *Server) forgotten(id string) {
	s.mu.Lock()
	delete(s.acts, id)
	s.mu.Unlock()
	s.usage.forget(id)
	s.fleetPublish("session_removed", map[string]string{"id": id})
}

// defaultSessionRetention is how long an ended session is kept after it
// ended, unless coordinator.json's session_retention_days says otherwise.
const defaultSessionRetention = 90 * 24 * time.Hour

// maxSessionRetentionDays caps session_retention_days (100 years).
const maxSessionRetentionDays = 36500

// sessionRetention returns how long ended sessions are kept (0: forever).
func (s *Server) sessionRetention() time.Duration {
	switch d := s.cfg.SessionRetentionDays; {
	case d < 0:
		return 0
	case d == 0:
		return defaultSessionRetention
	case d > maxSessionRetentionDays:
		// Far past anything kept, and short of overflowing a Duration.
		return maxSessionRetentionDays * 24 * time.Hour
	default:
		return time.Duration(d) * 24 * time.Hour
	}
}

// ended reports whether sess ended (stopped, or gone from its node) more
// than keep ago, by the coordinator's clock. One with no end time, or
// still running or in error, is kept.
func ended(sess model.Session, now time.Time, keep time.Duration) bool {
	return store.Ended(sess) && !sess.EndedAt.IsZero() && now.Sub(sess.EndedAt) >= keep
}

// ancestorsOfLive returns the sessions above a live session in the spawn
// chain. They are kept however old: the spawn depth of the live ones is
// counted through them.
func ancestorsOfLive(sessions []model.Session) map[string]bool {
	byID := make(map[string]model.Session, len(sessions))
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}
	keep := map[string]bool{}
	for _, sess := range sessions {
		if store.Ended(sess) {
			continue
		}
		for p, d := sess.Parent, 0; p != "" && d < 32 && !keep[p]; d++ {
			keep[p] = true
			p = byID[p].Parent
		}
	}
	return keep
}

// pruneSessions retires ended sessions past the retention: their records
// and event logs go, and tombstones keep their nodes from bringing them
// back. Without it the store would grow with every session ever run. Each
// call takes at most one batch per host, so a large first prune is spread
// over several minutes rather than holding up the coordinator.
func (s *Server) pruneSessions(now time.Time) {
	// Said when the count changes, not every minute.
	if n := s.store.RemoveDeletedLogs(); n != s.pendingLogs {
		if n > 0 {
			s.logf("%d event logs of deleted sessions could not be removed yet; trying again every minute", n)
		}
		s.pendingLogs = n
	}
	keep := s.sessionRetention()
	if keep == 0 {
		return
	}
	sessions := s.store.Sessions()
	ancestors := ancestorsOfLive(sessions)
	byHost := map[string][]string{}
	for _, sess := range sessions {
		if ended(sess, now, keep) && !ancestors[sess.ID] {
			byHost[sess.Host] = append(byHost[sess.Host], sess.ID)
		}
	}
	n := 0
	for host, ids := range byHost {
		n += len(s.retire(host, ids, func(cur model.Session) bool { return ended(cur, now, keep) }))
	}
	if n > 0 {
		s.logf("removed %d sessions that ended more than %d days ago", n, int(keep/(24*time.Hour)))
	}
}

// retire removes host's sessions in ids that still pass ok, checked again
// with the host's events held back (so a session that just came back is
// not taken from under them) and with no live session spawned below them.
// It returns those removed.
func (s *Server) retire(host string, ids []string, ok func(model.Session) bool) []string {
	defer s.lockIngest(host)()
	ancestors := ancestorsOfLive(s.store.Sessions())
	var batch []string
	for _, id := range ids {
		if cur, found := s.store.Session(id); found && cur.Host == host && ok(cur) && !ancestors[id] {
			batch = append(batch, id)
		}
	}
	if len(batch) == 0 {
		return nil
	}
	removed, err := s.store.RetireSessions(batch)
	if err != nil {
		s.logf("remove sessions of %s: %v", host, err)
		return nil
	}
	for _, id := range removed {
		s.forgotten(id)
	}
	return removed
}

// RemoveSession retires an ended session now, as pruneSessions would after
// the retention. A live one must be stopped first, and one with live
// sessions spawned below it is kept while they run.
func (s *Server) RemoveSession(id string) error {
	sess, ok := s.store.Session(id)
	if !ok {
		return store.ErrNotFound
	}
	if !store.Ended(sess) {
		return fmt.Errorf("%w: %s is %s; stop it first", errLive, sess.Path, sess.Status)
	}
	if ancestorsOfLive(s.store.Sessions())[id] {
		return fmt.Errorf("%w: %s has sessions spawned below it that still run; stop them first", errLive, sess.Path)
	}
	if len(s.retire(sess.Host, []string{id}, store.Ended)) == 0 {
		if _, still := s.store.Session(id); !still {
			return store.ErrNotFound
		}
		return fmt.Errorf("could not remove %s (see the coordinator's log)", sess.Path)
	}
	s.logf("removed %s (%s)", sess.Path, id)
	return nil
}

// errLive is returned when removing a session that has not ended, or that
// live sessions were spawned below.
var errLive = errors.New("session is live")

// purgeFailedSpawns removes sessions left behind by spawns that failed
// before this coordinator forgot them, or that were in flight when it
// stopped: in error or starting, never registered by a harness, and
// without a single event. A node that did start one after all reports it
// again and it is adopted.
func (s *Server) purgeFailedSpawns() {
	for _, sess := range s.store.Sessions() {
		if (sess.Status == model.StatusError || sess.Status == model.StatusStarting) && sess.NativeRef == nil && s.store.LastSeq(sess.ID) == 0 {
			if err := s.store.DeleteSession(sess.ID); err != nil {
				s.logf("purge failed spawn %s: %v", sess.Path, err)
				continue
			}
			s.logf("removed %s (%s): its spawn failed", sess.Path, sess.ID)
		}
	}
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
