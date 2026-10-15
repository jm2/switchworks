// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/wire"
)

// Guards bound what agents can do to each other and to the budget.
type Guards struct {
	MaxHops           int   `json:"max_hops,omitempty"`            // longest chain of message-triggered turns (default 8)
	AskPerMinute      int   `json:"ask_per_minute,omitempty"`      // per sender -> target pair (default 10)
	SendPerMinute     int   `json:"send_per_minute,omitempty"`     // per sender -> target pair (default 30)
	OutPerMinute      int   `json:"out_per_minute,omitempty"`      // per sender, all messages (default 60)
	MaxSpawnDepth     int   `json:"max_spawn_depth,omitempty"`     // sessions spawned by spawned sessions... (default 2)
	MaxDailyTokens    int64 `json:"max_daily_tokens,omitempty"`    // fleet-wide; refuses fleet_spawn once reached (0 = off)
	DefaultTTLSeconds int   `json:"default_ttl_seconds,omitempty"` // undelivered messages expire (default 24h)

	// What agents may spawn. A child always inherits its parent's approval
	// policy and sandbox.
	SpawnHosts     []string `json:"spawn_hosts,omitempty"`      // hosts agents may spawn on; empty = the agent's own host, "*" = any
	SpawnHarnesses []string `json:"spawn_harnesses,omitempty"`  // harnesses besides the agent's own; "*" = any
	SpawnCwdRoots  []string `json:"spawn_cwd_roots,omitempty"`  // directories besides the agent's own cwd (and below); "*" = any
	SpawnPerMinute int      `json:"spawn_per_minute,omitempty"` // per agent (default 3)

	// What agents may do to sessions they did not spawn.
	AllowStopAny  bool `json:"allow_stop_any,omitempty"`  // stop them
	AllowSteerAny bool `json:"allow_steer_any,omitempty"` // steer or interrupt their running turn
	AllowReadAny  bool `json:"allow_read_any,omitempty"`  // read their last message with fleet_status
	MaxBroadcast  int  `json:"max_broadcast,omitempty"`   // recipients of one broadcast from an agent (default 8)
	// AllowUnsandboxedSpawn lets a sandboxed agent spawn a harness that
	// does not enforce sandboxes (ACP agents such as goose).
	AllowUnsandboxedSpawn bool `json:"allow_unsandboxed_spawn,omitempty"`
}

func (g Guards) withDefaults() Guards {
	if g.MaxHops <= 0 {
		g.MaxHops = 8
	}
	if g.AskPerMinute <= 0 {
		g.AskPerMinute = 10
	}
	if g.SendPerMinute <= 0 {
		g.SendPerMinute = 30
	}
	if g.MaxSpawnDepth <= 0 {
		g.MaxSpawnDepth = 2
	}
	if g.DefaultTTLSeconds <= 0 {
		g.DefaultTTLSeconds = 24 * 3600
	}
	if g.OutPerMinute <= 0 {
		g.OutPerMinute = 60
	}
	if g.SpawnPerMinute <= 0 {
		g.SpawnPerMinute = 3
	}
	if g.MaxBroadcast <= 0 {
		g.MaxBroadcast = 8
	}
	return g
}

// Errors returned to senders.
var (
	ErrHopLimit  = errors.New("hop limit reached: this message would extend a chain of agent-to-agent turns past the configured maximum")
	ErrRateLimit = errors.New("rate limit: too many messages to this target; wait a minute")
	ErrBudget    = errors.New("daily token budget reached; spawning is paused until tomorrow (UTC)")
)

// sender is who a message is from: a session, a person, or Switchyard.
type sender struct {
	sess *model.Session
	user string // "user:<name>"
}

func (s sender) addr() string {
	switch {
	case s.sess != nil:
		return s.sess.Path
	case s.user != "":
		return s.user
	}
	return model.SystemSender
}

func (s sender) sessionID() string {
	if s.sess != nil {
		return s.sess.ID
	}
	return ""
}

type target struct {
	sess *model.Session
	user string
}

// trigger is what started a session's current work.
type trigger struct {
	hops int
	msg  string // message id, if a routed message
}

type bucket struct {
	tokens float64
	last   time.Time
}

type router struct {
	s      *Server
	guards Guards

	mu       sync.Mutex
	waiters  map[string][]chan model.Message // ask id -> waiting callers
	inbound  map[string]trigger              // session id -> what started its current work
	buckets  map[string]*bucket
	inflight map[string]bool

	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newRouter(s *Server, g Guards) *router {
	r := &router{s: s, guards: g.withDefaults(), waiters: map[string][]chan model.Message{},
		inbound: map[string]trigger{}, buckets: map[string]*bucket{}, inflight: map[string]bool{},
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	return r
}

func (r *router) close() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *router) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// loop retries pending messages and expires stale ones.
func (r *router) loop() {
	defer close(r.done)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var lastTidy time.Time
	for {
		select {
		case <-t.C:
		case <-r.wake:
		case <-r.stop:
			return
		}
		now := time.Now()
		if now.Sub(lastTidy) > time.Minute {
			lastTidy = now
			r.tidy(now)
		}
		for _, m := range r.s.store.PendingMessages() {
			select {
			case <-r.stop:
				return
			default:
			}
			if now.After(m.ExpiresAt) {
				r.expire(m)
				continue
			}
			if now.Before(m.NotBefore) {
				continue
			}
			if m.ToSession != "" {
				if sess, ok := r.s.store.Session(m.ToSession); ok && !r.s.Online(sess.Host) {
					continue
				}
			}
			// Deliver concurrently so one slow host does not hold up the
			// rest; deliver itself skips messages already in flight.
			go func(m model.Message) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				r.deliver(ctx, m)
			}(m)
		}
	}
}

// tidy drops state that would otherwise grow for as long as the
// coordinator runs: rate-limit buckets that have refilled, triggers of
// sessions that have ended, and finished messages past their retention.
func (r *router) tidy(now time.Time) {
	r.mu.Lock()
	for k, b := range r.buckets {
		if now.Sub(b.last) > 2*time.Minute {
			delete(r.buckets, k)
		}
	}
	ids := make([]string, 0, len(r.inbound))
	for id := range r.inbound {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		if sess, ok := r.s.store.Session(id); !ok || !live(sess) {
			r.mu.Lock()
			delete(r.inbound, id)
			r.mu.Unlock()
		}
	}
	r.s.store.PruneMessages(now)
}

// put stores a message and tells the fleet views.
func (r *router) put(m model.Message) error {
	if err := r.s.store.PutMessage(m); err != nil {
		return err
	}
	r.s.fleetPublish("message", m)
	return nil
}

func live(s model.Session) bool {
	return s.Status != model.StatusStopped && s.Status != model.StatusGone
}

// resolve turns an address into targets. Addresses: a session id,
// "host/name", a fleet-unique "name", "parent", "@key=value[,key=value]"
// (labels, plus the pseudo-labels host and harness), or "user:<name>".
func (r *router) resolve(from sender, to string, broadcast bool) ([]target, error) {
	to = strings.TrimSpace(to)
	switch {
	case to == "":
		return nil, errors.New("missing recipient (to)")
	case strings.HasPrefix(to, "user:"):
		if err := model.ValidateName("user", strings.TrimPrefix(to, "user:")); err != nil {
			return nil, err
		}
		return []target{{user: to}}, nil
	case to == "parent":
		if from.sess == nil || from.sess.Parent == "" {
			return nil, errors.New("this session has no parent")
		}
		p, ok := r.s.store.Session(from.sess.Parent)
		if !ok {
			return nil, errors.New("parent session not found")
		}
		return []target{{sess: &p}}, nil
	case strings.HasPrefix(to, "@"):
		want := map[string]string{}
		for _, kv := range strings.Split(strings.TrimPrefix(to, "@"), ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("bad selector %q: use @key=value[,key=value]", to)
			}
			want[k] = v
		}
		var matches []model.Session
		for _, sess := range r.s.store.Sessions() {
			if !live(sess) || sess.Status == model.StatusError || sess.ID == from.sessionID() {
				continue
			}
			if selectorMatch(sess, want) {
				matches = append(matches, sess)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no live session matches %s", to)
		}
		rank := func(s model.Session) int {
			switch {
			case r.s.Online(s.Host) && s.Status == model.StatusIdle:
				return 0
			case r.s.Online(s.Host):
				return 1
			}
			return 2
		}
		sort.SliceStable(matches, func(i, j int) bool {
			ri, rj := rank(matches[i]), rank(matches[j])
			if ri != rj {
				return ri < rj
			}
			return matches[i].Path < matches[j].Path
		})
		if !broadcast {
			matches = matches[:1]
		}
		out := make([]target, len(matches))
		for i := range matches {
			out[i] = target{sess: &matches[i]}
		}
		return out, nil
	}
	sess, err := r.lookup(to)
	if err != nil {
		return nil, err
	}
	return []target{{sess: &sess}}, nil
}

func selectorMatch(s model.Session, want map[string]string) bool {
	for k, v := range want {
		var got string
		switch k {
		case "host":
			got = s.Host
		case "harness":
			got = s.Harness
		default:
			got = s.Labels[k]
		}
		if got != v {
			return false
		}
	}
	return true
}

// lookup finds one session by id, path or fleet-unique name, preferring
// live sessions.
func (r *router) lookup(ref string) (model.Session, error) {
	if sess, ok := r.s.store.Session(ref); ok {
		return sess, nil
	}
	if sess, ok := r.s.store.SessionByPath(ref); ok {
		return sess, nil
	}
	if strings.Contains(ref, "/") {
		return model.Session{}, fmt.Errorf("no session %q", ref)
	}
	var found []model.Session
	for _, sess := range r.s.store.Sessions() {
		if sess.Name == ref && live(sess) {
			found = append(found, sess)
		}
	}
	switch len(found) {
	case 0:
		return model.Session{}, fmt.Errorf("no live session named %q", ref)
	case 1:
		return found[0], nil
	}
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.Path
	}
	return model.Session{}, fmt.Errorf("%q is ambiguous (%s); use host/name", ref, strings.Join(paths, ", "))
}

func (r *router) hopsFor(from sender) int {
	if from.sess == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inbound[from.sess.ID].hops + 1
}

// started records that a session's work was started by a spawn at the
// given hop count.
func (r *router) started(sessionID string, hops int) {
	r.mu.Lock()
	r.inbound[sessionID] = trigger{hops: hops}
	r.mu.Unlock()
}

// allow applies the per-pair rate limit (token bucket).
func (r *router) allow(key string, perMinute int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b := r.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(perMinute), last: now}
		r.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * float64(perMinute)
	if b.tokens > float64(perMinute) {
		b.tokens = float64(perMinute)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// send routes a notify, ask or broadcast. It persists each message and
// tries to deliver it before returning.
func (r *router) send(ctx context.Context, from sender, kind string, args fleet.SendArgs, opts ...func(*model.Message)) ([]model.Message, error) {
	if strings.TrimSpace(args.Text) == "" {
		return nil, errors.New("text is required")
	}
	if len(args.Text) > fleet.MaxTextBytes {
		return nil, fmt.Errorf("text is %d bytes; the limit is %d", len(args.Text), fleet.MaxTextBytes)
	}
	switch args.Mode {
	case "", model.ModeQueue, model.ModeSteer, model.ModeInterrupt:
	default:
		return nil, fmt.Errorf("unknown mode %q (queue, steer, interrupt)", args.Mode)
	}
	if kind == model.MsgBroadcast {
		args.Broadcast = true
		kind = model.MsgNotify
	}
	if kind == model.MsgAsk && args.Broadcast {
		return nil, errors.New("an ask goes to exactly one recipient")
	}
	targets, err := r.resolve(from, args.To, args.Broadcast)
	if err != nil {
		return nil, err
	}
	if from.sess != nil {
		if len(targets) > r.guards.MaxBroadcast {
			return nil, fmt.Errorf("%s matches %d sessions; agents may broadcast to at most %d", args.To, len(targets), r.guards.MaxBroadcast)
		}
		if args.Mode == model.ModeSteer || args.Mode == model.ModeInterrupt {
			for _, t := range targets {
				if t.sess != nil && t.sess.Parent != from.sess.ID && !r.guards.AllowSteerAny {
					return nil, fmt.Errorf("agents may %s only sessions they spawned; send it queued instead", args.Mode)
				}
			}
		}
	}
	hops := r.hopsFor(from)
	if hops > r.guards.MaxHops {
		return nil, ErrHopLimit
	}
	ttl := time.Duration(args.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(r.guards.DefaultTTLSeconds) * time.Second
	}
	group := ""
	if len(targets) > 1 {
		group = ulid.New()
	}
	now := time.Now().UTC()
	var out []model.Message
	for _, t := range targets {
		if t.sess != nil && t.sess.ID == from.sessionID() {
			return out, errors.New("a session cannot message itself")
		}
		if from.sess != nil {
			limit, class := r.guards.SendPerMinute, "send"
			if kind == model.MsgAsk {
				limit, class = r.guards.AskPerMinute, "ask"
			}
			// People share one bucket: names are free to invent, so a
			// bucket per name would be no limit at all.
			key := class + "|" + from.sess.ID + "|user:*"
			if t.sess != nil {
				key = class + "|" + from.sess.ID + "|" + t.sess.ID
			}
			if !r.allow(key, limit) {
				return out, ErrRateLimit
			}
			// Every message also counts against the sender's own budget,
			// whatever its recipients, so neither a broadcast nor many
			// recipients can get past it.
			if !r.allow("out|"+from.sess.ID, r.guards.OutPerMinute) {
				return out, ErrRateLimit
			}
		}
		m := model.Message{ID: ulid.New(), Kind: kind, From: from.addr(), FromSession: from.sessionID(),
			To: args.To, Text: args.Text, Mode: args.Mode, Group: group, Hops: hops, State: model.MsgPending,
			CreatedAt: now, ExpiresAt: now.Add(ttl)}
		if t.sess != nil {
			m.ToSession, m.ToPath = t.sess.ID, t.sess.Path
		} else {
			m.ToUser, m.ToPath = t.user, t.user
		}
		for _, o := range opts {
			o(&m)
		}
		if err := r.put(m); err != nil {
			return out, err
		}
		out = append(out, r.deliver(ctx, m))
	}
	return out, nil
}

// errRejected marks errors the node itself returned (the command reached
// it and it refused), as opposed to transport failures.
type errRejected struct{ msg string }

func (e *errRejected) Error() string { return e.msg }

// deliver tries to hand one pending message to its target, updating and
// returning its record.
func (r *router) deliver(ctx context.Context, m model.Message) model.Message {
	r.mu.Lock()
	if r.inflight[m.ID] {
		r.mu.Unlock()
		return m
	}
	r.inflight[m.ID] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, m.ID)
		r.mu.Unlock()
	}()
	if cur, ok := r.s.store.Message(m.ID); ok {
		m = cur
	}
	if m.State != model.MsgPending {
		return m
	}
	if m.ToUser != "" {
		m.State, m.DeliveredAt = model.MsgDelivered, time.Now().UTC()
		_ = r.put(m)
		return m
	}
	sess, ok := r.s.store.Session(m.ToSession)
	if !ok || !live(sess) {
		return r.fail(m, "target session is stopped or gone")
	}
	if !r.s.Online(sess.Host) {
		return m
	}
	res, err := r.s.command(ctx, sess.Host, wire.Cmd{Op: wire.OpDeliver, Session: sess.ID, Message: &wire.MessageCmd{
		ID: m.ID, From: m.From, Text: m.Text, Mode: m.Mode, Kind: m.Kind, ReplyTo: m.ReplyTo}}, 30*time.Second)
	if err != nil {
		var rej *errRejected
		if errors.As(err, &rej) {
			return r.fail(m, rej.msg)
		}
		return m // transport problem: stays pending, retried by the loop
	}
	var dr wire.DeliverResult
	_ = json.Unmarshal(res.Data, &dr)
	m.State, m.DeliveredAt = model.MsgDelivered, time.Now().UTC()
	if dr.Mode != "" {
		m.Mode = dr.Mode
	}
	if cur, ok := r.s.store.Message(m.ID); ok && cur.State == model.MsgAnswered {
		return cur // answered while in flight
	}
	_ = r.put(m)
	return m
}

func (r *router) fail(m model.Message, why string) model.Message {
	m.State, m.Error = model.MsgFailed, why
	_ = r.put(m)
	r.notifySender(m, fmt.Sprintf("Your %s message %s to %s could not be delivered: %s", m.Kind, m.ID, m.To, why))
	return m
}

func (r *router) expire(m model.Message) {
	m.State, m.Error = model.MsgExpired, "expired before it could be delivered"
	_ = r.put(m)
	r.notifySender(m, fmt.Sprintf("Your %s message %s to %s expired before it could be delivered (its host was offline).", m.Kind, m.ID, m.To))
}

// notifySender queues a system notice for the sending session. Notices are
// never sent about notices, completion messages or replies.
func (r *router) notifySender(m model.Message, text string) {
	if m.FromSession == "" || m.Kind == model.MsgSystem || m.Kind == model.MsgDone {
		return
	}
	sess, ok := r.s.store.Session(m.FromSession)
	if !ok || !live(sess) {
		return
	}
	n := model.Message{ID: ulid.New(), Kind: model.MsgSystem, From: model.SystemSender, To: sess.Path,
		ToSession: sess.ID, ToPath: sess.Path, Text: text, Hops: m.Hops, State: model.MsgPending,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Duration(r.guards.DefaultTTLSeconds) * time.Second)}
	if r.put(n) == nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.deliver(ctx, n)
		}()
	}
}

// ask sends an ask and, if wait > 0, waits for the reply.
func (r *router) ask(ctx context.Context, from sender, args fleet.AskArgs) fleet.AskResult {
	wait := fleet.DefaultAskWait
	if args.WaitSeconds != nil {
		wait = time.Duration(*args.WaitSeconds) * time.Second
	}
	if wait > fleet.MaxAskWait {
		wait = fleet.MaxAskWait
	}
	var opts []func(*model.Message)
	if wait <= 0 && args.HoldSeconds > 0 {
		hold := time.Duration(args.HoldSeconds) * time.Second
		if hold > fleet.MaxAskWait+5*time.Minute {
			hold = fleet.MaxAskWait + 5*time.Minute
		}
		opts = append(opts, func(m *model.Message) { m.HoldUntil = m.CreatedAt.Add(hold) })
	}
	msgs, err := r.send(ctx, from, model.MsgAsk, fleet.SendArgs{To: args.To, Text: args.Text, TTLSeconds: args.TTLSeconds}, opts...)
	if err != nil {
		return fleet.AskResult{Status: fleet.AskFailed, Error: err.Error()}
	}
	m := msgs[0]
	res := fleet.AskResult{Status: fleet.AskPending, MessageID: m.ID, Target: m.ToPath}
	if m.State == model.MsgFailed {
		res.Status, res.Error = fleet.AskFailed, m.Error
		return res
	}
	if wait > 0 {
		if rep := r.waitReply(ctx, m.ID, wait); rep != nil {
			res.Status, res.Reply, res.ReplyFrom = fleet.AskReplied, rep.Text, rep.From
			return res
		}
	}
	res.Note = "No reply yet. It will arrive later as a message in this session; or call fleet_inbox with ticket=" + m.ID + "."
	return res
}

// waitReply blocks until askID is answered, d elapses or ctx ends.
func (r *router) waitReply(ctx context.Context, askID string, d time.Duration) *model.Message {
	r.mu.Lock()
	if ask, ok := r.s.store.Message(askID); ok && ask.ReplyID != "" {
		defer r.mu.Unlock()
		rep, ok := r.s.store.Message(ask.ReplyID)
		if !ok {
			return nil
		}
		if rep.State == model.MsgPending && time.Now().Before(rep.NotBefore) {
			// A held reply, collected by the asker: it need not be
			// delivered as a turn.
			rep.State, rep.Inline, rep.DeliveredAt = model.MsgDelivered, true, time.Now().UTC()
			_ = r.put(rep)
		}
		return &rep
	}
	ch := make(chan model.Message, 1)
	r.waiters[askID] = append(r.waiters[askID], ch)
	r.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case rep := <-ch:
		return &rep
	case <-timer.C:
	case <-ctx.Done():
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ws := r.waiters[askID]
	for i, w := range ws {
		if w == ch {
			r.waiters[askID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(r.waiters[askID]) == 0 {
		delete(r.waiters, askID)
	}
	select {
	case rep := <-ch:
		return &rep
	default:
		return nil
	}
}

// reply answers an ask. A caller waiting in fleet_ask gets the reply
// directly; otherwise it is delivered as a message to the asker.
func (r *router) reply(ctx context.Context, from sender, args fleet.ReplyArgs) (fleet.ReplyResult, error) {
	if strings.TrimSpace(args.Text) == "" {
		return fleet.ReplyResult{}, errors.New("text is required")
	}
	if len(args.Text) > fleet.MaxTextBytes {
		return fleet.ReplyResult{}, fmt.Errorf("text is %d bytes; the limit is %d", len(args.Text), fleet.MaxTextBytes)
	}
	ask, ok := r.s.store.Message(args.MessageID)
	if !ok || ask.Kind != model.MsgAsk {
		return fleet.ReplyResult{}, fmt.Errorf("no ask with id %q", args.MessageID)
	}
	switch {
	case from.sess != nil && ask.ToSession == from.sess.ID:
	case from.user != "" && ask.ToUser != "":
	default:
		return fleet.ReplyResult{}, errors.New("only the recipient of an ask can reply to it")
	}
	now := time.Now().UTC()
	rep := model.Message{ID: ulid.New(), Kind: model.MsgReply, From: from.addr(), FromSession: from.sessionID(),
		To: ask.From, ReplyTo: ask.ID, Text: args.Text, Hops: ask.Hops, State: model.MsgPending,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(r.guards.DefaultTTLSeconds) * time.Second)}
	switch {
	case ask.FromSession != "":
		if s, ok := r.s.store.Session(ask.FromSession); ok {
			rep.ToSession, rep.ToPath = s.ID, s.Path
		}
	case strings.HasPrefix(ask.From, "user:"):
		rep.ToUser, rep.ToPath = ask.From, ask.From
	}

	r.mu.Lock()
	if cur, ok := r.s.store.Message(ask.ID); ok && cur.ReplyID != "" {
		r.mu.Unlock()
		return fleet.ReplyResult{}, errors.New("this ask has already been answered")
	}
	ws := r.waiters[ask.ID]
	delete(r.waiters, ask.ID)
	held := false
	switch {
	case len(ws) > 0:
		rep.State, rep.Inline, rep.DeliveredAt = model.MsgDelivered, true, now
	case now.Before(ask.HoldUntil) && rep.ToSession != "":
		rep.NotBefore, held = ask.HoldUntil, true
	}
	if err := r.put(rep); err != nil {
		r.mu.Unlock()
		return fleet.ReplyResult{}, err
	}
	if cur, ok := r.s.store.Message(ask.ID); ok {
		ask = cur
	}
	ask.State, ask.ReplyID = model.MsgAnswered, rep.ID
	_ = r.put(ask)
	for _, w := range ws {
		w <- rep
	}
	r.mu.Unlock()

	if !rep.Inline && !held {
		if rep.ToSession == "" && rep.ToUser == "" {
			rep = r.fail(rep, "the asker no longer exists")
		} else {
			rep = r.deliver(ctx, rep)
		}
	}
	res := fleet.ReplyResult{ReplyID: rep.ID, State: rep.State, Inline: rep.Inline}
	if held {
		res.State = fleet.ReplyHeld
	}
	return res, nil
}

// inbox reports on a ticket and lists recent messages for a session (or the
// human inbox when caller has no session).
func (r *router) inbox(ctx context.Context, from sender, args fleet.InboxArgs) (fleet.InboxResult, error) {
	var res fleet.InboxResult
	if args.Ticket != "" {
		ask, ok := r.s.store.Message(args.Ticket)
		mine := ask.FromSession == from.sessionID() && (from.sess != nil || ask.From == from.addr())
		if !ok || ask.Kind != model.MsgAsk || !mine {
			return res, fmt.Errorf("no ask %q from this session", args.Ticket)
		}
		t := &fleet.AskResult{Status: fleet.AskPending, MessageID: ask.ID, Target: ask.ToPath}
		switch ask.State {
		case model.MsgFailed, model.MsgExpired:
			t.Status, t.Error = fleet.AskFailed, ask.Error
		default:
			wait := time.Duration(args.WaitSeconds) * time.Second
			if wait > fleet.MaxAskWait {
				wait = fleet.MaxAskWait
			}
			var rep *model.Message
			if ask.ReplyID != "" || wait > 0 {
				rep = r.waitReply(ctx, ask.ID, wait)
			}
			if rep != nil {
				t.Status, t.Reply, t.ReplyFrom = fleet.AskReplied, rep.Text, rep.From
			}
		}
		res.Ticket = t
	}
	limit := args.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	filter := store.MessageFilter{Session: from.sessionID(), Limit: limit}
	if from.sess == nil {
		filter = store.MessageFilter{ToUser: true, Limit: limit}
	}
	msgs := r.s.store.Messages(filter)
	for i := range msgs {
		if len(msgs[i].Text) > 2000 {
			msgs[i].Text = msgs[i].Text[:2000] + "…"
		}
	}
	res.Messages = msgs
	if res.Messages == nil {
		res.Messages = []model.Message{}
	}
	return res, nil
}

// onDelivered records the hops of the message that started a session's
// next turn.
func (r *router) onDelivered(sessionID, messageID string) {
	m, ok := r.s.store.Message(messageID)
	if !ok {
		r.mu.Lock()
		r.inbound[sessionID] = trigger{} // not a routed message
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	r.inbound[sessionID] = trigger{hops: m.Hops, msg: m.ID}
	r.mu.Unlock()
}

// onSettled sends a completion notice to a spawned session's parent.
func (r *router) onSettled(child model.Session, p model.Settled) {
	if child.Parent == "" {
		return
	}
	parent, ok := r.s.store.Session(child.Parent)
	if !ok || !live(parent) {
		return
	}
	r.mu.Lock()
	tr := r.inbound[child.ID]
	r.mu.Unlock()
	hops := tr.hops + 1
	// The parent asked and has had its answer: a completion notice on top
	// would only repeat it.
	if tr.msg != "" {
		if m, ok := r.s.store.Message(tr.msg); ok && m.Kind == model.MsgAsk && m.FromSession == parent.ID && m.State == model.MsgAnswered {
			return
		}
	}
	if hops > r.guards.MaxHops {
		r.s.logf("router: not notifying %s that %s finished: hop limit", parent.Path, child.Path)
		return
	}
	last := p.LastMessage
	if len(last) > 4000 {
		last = last[:4000] + "…"
	}
	status := p.TurnStatus
	if status == "" {
		status = "completed"
	}
	text := fmt.Sprintf("Session %s, which you spawned, has finished its work (last turn: %s).", child.Path, status)
	if last != "" {
		text += "\n\nIts last message:\n" + last
	}
	now := time.Now().UTC()
	m := model.Message{ID: ulid.New(), Kind: model.MsgDone, From: child.Path, FromSession: child.ID, To: parent.Path,
		ToSession: parent.ID, ToPath: parent.Path, Text: text, Hops: hops, State: model.MsgPending,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(r.guards.DefaultTTLSeconds) * time.Second)}
	if r.put(m) == nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.deliver(ctx, m)
		}()
	}
}
