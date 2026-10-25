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
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
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
	// AgentNotifyTTLSeconds is the default expiry of an agent's notify
	// (default 2h); it may ask for longer, up to DefaultTTLSeconds.
	AgentNotifyTTLSeconds int `json:"agent_notify_ttl_seconds,omitempty"`

	// What agents may spawn. A child always inherits its parent's approval
	// policy and sandbox.
	SpawnHosts     []string `json:"spawn_hosts,omitempty"`      // hosts agents may spawn on; empty = the agent's own host, "*" = any
	SpawnHarnesses []string `json:"spawn_harnesses,omitempty"`  // harnesses besides the agent's own; "*" = any
	SpawnCwdRoots  []string `json:"spawn_cwd_roots,omitempty"`  // directories besides the agent's own cwd (and below); "*" = any
	SpawnPerMinute int      `json:"spawn_per_minute,omitempty"` // per agent (default 3)

	// What agents may do to sessions they did not spawn.
	AllowStopAny  bool `json:"allow_stop_any,omitempty"`  // stop them
	AllowSteerAny bool `json:"allow_steer_any,omitempty"` // interrupt their running turn, and steer one that does not accept steering
	AllowReadAny  bool `json:"allow_read_any,omitempty"`  // read their last message with fleet_status
	MaxBroadcast  int  `json:"max_broadcast,omitempty"`   // recipients of one broadcast from an agent (default 8)
	// AllowUnsandboxedSpawn lets a sandboxed agent spawn a harness that
	// does not enforce sandboxes (ACP agents such as goose).
	AllowUnsandboxedSpawn bool `json:"allow_unsandboxed_spawn,omitempty"`
	// AllowUnapprovedSpawn lets an agent whose approval policy is not
	// "never" spawn agy, which never asks for approval.
	AllowUnapprovedSpawn bool `json:"allow_unapproved_spawn,omitempty"`
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
	if g.AgentNotifyTTLSeconds <= 0 {
		g.AgentNotifyTTLSeconds = 2 * 3600
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

// sender is who a message is from: a session, a person, or Switchworks.
type sender struct {
	sess *model.Session
	user string // "user:<name>"
	// via names a person's credential ("admin" or "token:<name>"), and
	// owner says whether it is the owner's; set from the request, never
	// from what the client says.
	via   string
	owner bool
	token string // the API token's id behind via ("" for the admin token)
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
	addr string // the address it was named by
}

// trigger is what started a session's current work: the deepest hop count
// of the messages its current turn took in, and their ids.
type trigger struct {
	hops int
	msgs []string // routed messages the turn took in, oldest first
	// idle: that turn has ended, so the next message to start one starts
	// the count over.
	idle bool
}

// maxTriggerMsgs bounds the ids a trigger keeps (steers into a long turn).
const maxTriggerMsgs = 64

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

	// Deliveries started in the background, so close can wait for them
	// before the store is closed.
	bg     sync.WaitGroup
	closed bool // under mu: no more background deliveries
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
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.bg.Wait()
}

// deliverLater tries to deliver m in the background, unless the router is
// closing (the message stays pending, and is delivered after a restart).
func (r *router) deliverLater(m model.Message) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.bg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.deliver(ctx, m)
	}()
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
			r.deliverLater(m)
		}
	}
}

// tidy drops state that would otherwise grow for as long as the
// coordinator runs: rate-limit buckets that have refilled, triggers of
// sessions that have ended, finished messages past their retention, and
// ended sessions past theirs.
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
	r.s.pruneSessions(now)
	if r.s.blobs != nil {
		pinned := map[string]bool{}
		for _, m := range r.s.store.PendingMessages() {
			for _, a := range m.Attachments {
				pinned[a.SHA256] = true
			}
			for _, f := range m.Files {
				pinned[f.SHA256] = true
			}
		}
		r.s.blobs.prune(now, pinned)
	}
}

// storeAttachments validates images (type sniffed from the bytes, size
// limits) and keeps their bytes in the attachment store. It returns them
// with their data and metadata filled in.
func (s *Server) storeAttachments(atts []model.Attachment) ([]model.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	out, err := attach.Normalize(atts)
	if err != nil {
		return nil, err
	}
	if s.blobs == nil {
		return nil, errors.New("this coordinator has no attachment store")
	}
	for _, a := range out {
		if _, err := s.blobs.put(a.Data); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// storeFiles keeps normalized files' bytes in the attachment store, which
// images and files share (by digest, under the same retention and limits).
func (s *Server) storeFiles(files []model.Attachment) error {
	if len(files) == 0 {
		return nil
	}
	if s.blobs == nil {
		return errors.New("this coordinator has no attachment store")
	}
	for _, f := range files {
		if _, err := s.blobs.put(f.Data); err != nil {
			return err
		}
	}
	return nil
}

// filesRefusal says why files cannot go to a session on host, or returns
// nil.
func (s *Server) filesRefusal(host string) error {
	ok := false
	if n, online := s.node(host); online {
		ok = contains(n.hello.Features, wire.FeatureFiles)
	} else if h, found := s.store.Host(host); found {
		ok = h.Files
	}
	if !ok {
		return fmt.Errorf("sessions on %s cannot take files (its node does not offer them; an older swk node never does)", host)
	}
	return nil
}

// filesTo says why files cannot go to t, or returns nil. People cannot
// take them: they read messages in the web UI and the CLI, which show no
// files.
func (s *Server) filesTo(t target) error {
	if t.sess == nil {
		return fmt.Errorf("files go to agent sessions only, not to %s", t.user)
	}
	return s.filesRefusal(t.sess.Host)
}

// loadAttachments reads a stored message's images (or files) back for
// delivery.
func (s *Server) loadAttachments(metas []model.Attachment) ([]model.Attachment, error) {
	if s.blobs == nil {
		return nil, errors.New("this coordinator has no attachment store")
	}
	out := make([]model.Attachment, 0, len(metas))
	for _, m := range metas {
		data, err := s.blobs.get(m.SHA256)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", m.Name, err)
		}
		m.Data = data
		out = append(out, m)
	}
	return out, nil
}

// imagesRefusal says why images cannot go to harness on host (and, once it
// has registered, to session sess), or returns nil.
func (s *Server) imagesRefusal(host, harness string, sess *model.Session) error {
	if harness == "" {
		harness = "codex"
	}
	var list []string
	if n, ok := s.node(host); ok {
		list = n.hello.Images
	} else if h, ok := s.store.Host(host); ok {
		list = h.Images
	}
	switch {
	case harness == "agy":
		return errors.New("agy sessions cannot take images (agy's headless input is text only)")
	case !contains(list, harness):
		return fmt.Errorf("%s on %s cannot take images (its node does not offer images for it; an older swk node never does)", harness, host)
	case sess != nil && sess.NativeRef != nil && !sess.Caps.Images:
		return fmt.Errorf("%s cannot take images: its agent does not advertise image prompts (ACP promptCapabilities.image)", sess.Path)
	}
	return nil
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

// turnEnded records that a session's turn is over (it is idle): the next
// message to start a turn starts its hop count over.
func (r *router) turnEnded(sessionID string) {
	r.mu.Lock()
	if tr, ok := r.inbound[sessionID]; ok {
		tr.idle = true
		r.inbound[sessionID] = tr
	}
	r.mu.Unlock()
}

// allow applies the per-pair rate limit (token bucket).
func (r *router) allow(key string, perMinute int) bool { return r.allowN(key, perMinute, 1) }

// agentImageBytesPerMinute bounds the images one agent may send, so a busy
// agent cannot crowd everyone's images out of the coordinator's store.
const agentImageBytesPerMinute = 20 << 20

// agentFileBytesPerMinute does the same for files.
const agentFileBytesPerMinute = 8 << 20

// personFileBytesPerMinute bounds the files sent with one person's
// credential (the admin token, an owner or operate token), which fill
// receiving nodes' disks as much as an agent's do.
const personFileBytesPerMinute = 8 << 20

// allowN takes n tokens from key's bucket, which refills to perMinute a
// minute.
func (r *router) allowN(key string, perMinute int, n float64) bool {
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
	if b.tokens < n {
		return false
	}
	b.tokens -= n
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
	case "", model.ModeAuto, model.ModeQueue, model.ModeSteer, model.ModeInterrupt:
	default:
		return nil, fmt.Errorf("unknown mode %q (auto, queue, steer, interrupt)", args.Mode)
	}
	if kind == model.MsgBroadcast {
		args.Broadcast = true
		kind = model.MsgNotify
	}
	if kind == model.MsgAsk && args.Broadcast {
		return nil, errors.New("an ask goes to exactly one recipient")
	}
	supersedes := strings.TrimSpace(args.Supersedes)
	switch {
	case supersedes != "" && kind != model.MsgNotify:
		return nil, errors.New("supersedes is for notifies, not asks")
	case len(supersedes) > fleet.MaxSupersedesBytes || !utf8.ValidString(supersedes):
		return nil, fmt.Errorf("supersedes must be UTF-8 text of at most %d bytes", fleet.MaxSupersedesBytes)
	}
	targets, err := r.resolveTo(from, args)
	if err != nil {
		return nil, err
	}
	if len(args.Images) > 0 {
		// Paths are read by the sender's node, which replaces them with
		// attachments; an older node passes them on unread.
		return nil, errors.New("image paths cannot be read: the sending host runs an older swk node; upgrade it")
	}
	if len(args.Files) > 0 {
		return nil, errors.New("file paths cannot be read: the sending host runs an older swk node; upgrade it")
	}
	if len(args.FileAttachments) > 0 {
		for _, t := range targets {
			if err := r.s.filesTo(t); err != nil {
				return nil, err
			}
		}
	}
	if len(args.Attachments) > 0 {
		for _, t := range targets {
			if t.sess != nil {
				if err := r.s.imagesRefusal(t.sess.Host, t.sess.Harness, t.sess); err != nil {
					return nil, err
				}
			}
		}
	}
	if from.sess != nil {
		if len(targets) > r.guards.MaxBroadcast {
			return nil, fmt.Errorf("%s matches %d sessions; agents may broadcast to at most %d", args.To, len(targets), r.guards.MaxBroadcast)
		}
		// Steering only adds input to a turn, so agents may steer any
		// session; interrupting cancels its work.
		if args.Mode == model.ModeInterrupt {
			for _, t := range targets {
				if t.sess != nil && t.sess.Parent != from.sess.ID && !r.guards.AllowSteerAny {
					return nil, errors.New("agents may interrupt only sessions they spawned; send it without a mode (auto) instead")
				}
			}
		}
	}
	hops := r.hopsFor(from)
	if hops > r.guards.MaxHops {
		return nil, ErrHopLimit
	}
	// The default is also the longest a message may wait: one that never
	// expires would be kept and rescanned forever. An agent's notify is
	// mostly news that goes stale, so it waits less unless it asks.
	ttl := time.Duration(args.TTLSeconds) * time.Second
	longest := time.Duration(r.guards.DefaultTTLSeconds) * time.Second
	if ttl <= 0 && from.sess != nil && kind == model.MsgNotify {
		ttl = time.Duration(r.guards.AgentNotifyTTLSeconds) * time.Second
	}
	if ttl <= 0 || ttl > longest {
		ttl = longest
	}
	group := ""
	if len(targets) > 1 {
		group = ulid.New()
	}
	// Every guard runs before anything is stored, so a refused send
	// costs no disk.
	for _, t := range targets {
		if t.sess != nil && t.sess.ID == from.sessionID() {
			return nil, errors.New("a session cannot message itself")
		}
	}
	var images []model.Attachment
	if len(args.Attachments) > 0 {
		if images, err = attach.Normalize(args.Attachments); err != nil {
			return nil, err
		}
	}
	files, err := attach.NormalizeFiles(args.FileAttachments)
	if err != nil {
		return nil, err
	}
	if from.sess != nil {
		for _, t := range targets {
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
				return nil, ErrRateLimit
			}
			// Every message also counts against the sender's own budget,
			// whatever its recipients, so neither a broadcast nor many
			// recipients can get past it.
			if !r.allow("out|"+from.sess.ID, r.guards.OutPerMinute) {
				return nil, ErrRateLimit
			}
		}
		if n := attach.TotalBytes(images); n > 0 && !r.allowN("img|"+from.sess.ID, agentImageBytesPerMinute, float64(n)) {
			return nil, fmt.Errorf("%w: agents may send at most %d MiB of images a minute", ErrRateLimit, agentImageBytesPerMinute>>20)
		}
		if n := attach.TotalBytes(files); n > 0 && !r.allowN("file|"+from.sess.ID, agentFileBytesPerMinute, float64(n)) {
			return nil, fmt.Errorf("%w: agents may send at most %d MiB of files a minute", ErrRateLimit, agentFileBytesPerMinute>>20)
		}
	} else if n := attach.TotalBytes(files); n > 0 {
		// People's files, by credential: the names they send as are free
		// to invent.
		key := from.via
		if key == "" {
			key = from.addr()
		}
		if !r.allowN("file|via:"+key, personFileBytesPerMinute, float64(n)) {
			return nil, fmt.Errorf("%w: each credential may send at most %d MiB of files a minute", ErrRateLimit, personFileBytesPerMinute>>20)
		}
	}
	if len(images) > 0 {
		if images, err = r.s.storeAttachments(images); err != nil {
			return nil, err
		}
	}
	if err := r.s.storeFiles(files); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out []model.Message
	for _, t := range targets {
		m := model.Message{ID: ulid.New(), Kind: kind, From: from.addr(), FromSession: from.sessionID(), Via: from.via, Owner: from.owner, ViaToken: from.token,
			To: t.addr, Text: args.Text, Attachments: model.MetaOnly(images), Mode: args.Mode, Group: group, Hops: hops,
			State: model.MsgPending, CreatedAt: now, ExpiresAt: now.Add(ttl), Supersedes: supersedes}
		m.Files = model.MetaOnly(files)
		if t.sess != nil {
			m.ToSession, m.ToPath = t.sess.ID, t.sess.Path
			m.Mode = r.steerMode(from, *t.sess, m.Mode)
		} else {
			m.ToUser, m.ToPath = t.user, t.user
		}
		for _, o := range opts {
			o(&m)
		}
		if err := r.put(m); err != nil {
			return out, err
		}
		if supersedes != "" && m.ToSession != "" {
			r.supersede(ctx, m) // before m goes, so the two never share a turn
		}
		out = append(out, r.deliver(ctx, m))
	}
	return out, nil
}

// supersede replaces the earlier messages m supersedes: those with its
// key from the same sender (for a person, the same credential) to the
// same session, still pending at the coordinator or queued at the
// session's node, which is told to drop them. Each becomes superseded,
// naming m. One that has gone into a turn, or that is being handed to
// the node at this moment, is left as it is, as is one queued at a node
// that is offline or older than 0.1.5.
func (r *router) supersede(ctx context.Context, m model.Message) {
	var queued []model.Message
	for _, old := range r.s.store.Messages(store.MessageFilter{Session: m.ToSession}) {
		if old.ID >= m.ID || old.Supersedes != m.Supersedes || old.ToSession != m.ToSession || old.Kind != model.MsgNotify ||
			old.From != m.From || old.FromSession != m.FromSession || old.Via != m.Via || old.ViaToken != m.ViaToken {
			continue
		}
		switch {
		case old.State == model.MsgPending:
			r.superseded(old.ID, m.ID, false)
		case old.State == model.MsgDelivered && old.DeliveredAt.IsZero() && old.SeenAt.IsZero():
			queued = append(queued, old)
		}
	}
	sess, ok := r.s.store.Session(m.ToSession)
	if !ok {
		return
	}
	for _, old := range queued {
		cmd := wire.Cmd{Op: wire.OpCancel, Queued: &wire.QueuedCmd{MessageID: old.ID, SupersededBy: m.ID}}
		if _, err := r.s.queueCommand(ctx, sess, cmd); err == nil {
			r.superseded(old.ID, m.ID, true)
		}
	}
}

// superseded marks a pending message (with queued, also one its node had
// queued and has dropped) superseded by the message by, unless a delivery
// of a pending one is under way. Its sender is not told: it sent the
// newer one.
func (r *router) superseded(id, by string, queued bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || !(cur.State == model.MsgPending && !r.inflight[id] || queued && cur.State == model.MsgDelivered) {
		return
	}
	cur.State, cur.SupersededBy = model.MsgSuperseded, by
	_ = r.put(cur)
}

// steerMode is the mode a message to sess goes with: a session that does
// not accept steering gets messages in mode auto, and an agent's steers
// unless the agent spawned it (or allow_steer_any is set), queued.
func (r *router) steerMode(from sender, sess model.Session, mode string) string {
	if sess.Steerable() {
		return mode
	}
	switch {
	case mode == model.ModeAuto:
		return model.ModeQueue
	case mode == model.ModeSteer && from.sess != nil && sess.Parent != from.sess.ID && !r.guards.AllowSteerAny:
		return model.ModeQueue
	}
	return mode
}

// resolveTo resolves a send's recipients: To, or each address in ToList.
// A session (or person) named by two addresses gets one message.
func (r *router) resolveTo(from sender, args fleet.SendArgs) ([]target, error) {
	var out []target
	seen := map[string]bool{}
	for _, to := range args.Recipients() {
		ts, err := r.resolve(from, to, args.Broadcast)
		if err != nil {
			if len(args.ToList) > 0 {
				return nil, fmt.Errorf("%s: %w", to, err)
			}
			return nil, err
		}
		for _, t := range ts {
			key := t.user
			if t.sess != nil {
				key = t.sess.ID
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			t.addr = to
			out = append(out, t)
		}
	}
	if from.sess != nil && len(args.ToList) > 0 && len(out) > r.guards.MaxBroadcast {
		return nil, fmt.Errorf("these %d addresses reach %d recipients; agents may send to at most %d at once", len(args.ToList), len(out), r.guards.MaxBroadcast)
	}
	return out, nil
}

// sendEach is send for a request whose "to" may list several addresses.
// An address that cannot take the message (unknown, the sender itself, a
// harness that takes no images) is refused on its own and the rest still
// go, as one group; the result lists each. Limits that count the whole
// send (rate limits, max_broadcast, hops) still refuse all of it.
func (r *router) sendEach(ctx context.Context, from sender, kind string, args fleet.SendArgs) (fleet.SendResult, error) {
	var refused []fleet.MessageInfo
	if len(args.ToList) > 0 {
		var ok []string
		var why []string
		for _, to := range args.ToList {
			if err := r.precheck(from, kind, args, to); err != nil {
				refused = append(refused, fleet.MessageInfo{To: to, State: fleet.Refused, Error: err.Error()})
				why = append(why, to+": "+err.Error())
				continue
			}
			ok = append(ok, to)
		}
		switch len(ok) {
		case 0:
			return fleet.SendResult{}, fmt.Errorf("no recipient can take it (%s)", strings.Join(why, "; "))
		case 1:
			args.To, args.ToList = ok[0], nil
		default:
			args.To, args.ToList = "", ok
		}
	}
	msgs, err := r.send(ctx, from, kind, args)
	if err != nil {
		return fleet.SendResult{}, err
	}
	res := fleet.SendResult{Messages: append(infos(msgs), refused...)}
	if len(msgs) > 0 {
		res.Group = msgs[0].Group
	}
	return res, nil
}

// precheck reports why one address of a send to several cannot take it.
func (r *router) precheck(from sender, kind string, args fleet.SendArgs, to string) error {
	ts, err := r.resolve(from, to, args.Broadcast || kind == model.MsgBroadcast)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if len(args.FileAttachments) > 0 {
			if err := r.s.filesTo(t); err != nil {
				return err
			}
		}
		if t.sess == nil {
			continue
		}
		if t.sess.ID == from.sessionID() {
			return errors.New("a session cannot message itself")
		}
		if len(args.Attachments) > 0 {
			if err := r.s.imagesRefusal(t.sess.Host, t.sess.Harness, t.sess); err != nil {
				return err
			}
		}
	}
	return nil
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
	var images []model.Attachment
	if len(m.Attachments) > 0 {
		var err error
		if images, err = r.s.loadAttachments(m.Attachments); err != nil {
			return r.fail(m, err.Error())
		}
	}
	var files []model.Attachment
	if len(m.Files) > 0 {
		// Checked again: an older node would drop them unseen.
		err := r.s.filesRefusal(sess.Host)
		if err == nil {
			files, err = r.s.loadAttachments(m.Files)
		}
		if err != nil {
			return r.fail(m, err.Error())
		}
	}
	res, err := r.s.command(ctx, sess.Host, wire.Cmd{Op: wire.OpDeliver, Session: sess.ID, Message: &wire.MessageCmd{
		ID: m.ID, From: m.From, Via: m.Via, Owner: m.Owner, Text: m.Text, Mode: m.Mode, Kind: m.Kind, ReplyTo: m.ReplyTo,
		Attachments: images, Files: files, ExpiresAt: m.ExpiresAt}}, 30*time.Second)
	if err != nil {
		var rej *errRejected
		if errors.As(err, &rej) {
			return r.fail(m, rej.msg)
		}
		return m // transport problem: stays pending, retried by the loop
	}
	var dr wire.DeliverResult
	_ = json.Unmarshal(res.Data, &dr)
	if dr.Expired {
		// It expired while waiting to be steered in, and the node dropped it.
		r.expireQueued(m.ID)
		out, _ := r.s.store.Message(m.ID)
		return out
	}
	out, _ := r.settle(m.ID, func(cur *model.Message) {
		cur.State = model.MsgDelivered
		if !cur.SeenAt.IsZero() {
			cur.State = model.MsgSeen // its turn finished before this answer was read
		}
		switch dr.Mode {
		case model.ModeQueue, model.ModeInterrupt:
			// It waits in the session's queue: delivered_at comes with the
			// node's message.delivered, when its turn starts (entered).
		default:
			if cur.DeliveredAt.IsZero() {
				cur.DeliveredAt = time.Now().UTC()
			}
		}
		if dr.Mode != "" {
			cur.Mode = dr.Mode
		}
	})
	return out
}

// entered records when a message went into its session's input, as the
// node reports it (message.delivered): its delivered_at, if not set yet.
func (r *router) entered(sessionID, messageID string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(messageID)
	if !ok || cur.ToSession != sessionID || !cur.DeliveredAt.IsZero() {
		return
	}
	if cur.State != model.MsgPending && cur.State != model.MsgDelivered && cur.State != model.MsgAnswered {
		return
	}
	cur.DeliveredAt = nodeTime(at, cur.CreatedAt)
	_ = r.put(cur)
}

// onSeen marks a message seen when the turn that carried it has completed
// (message.seen), keeping the turn and its last agent message for the
// receipt. An answered ask stays answered; a message seen before the
// coordinator read the node's answer to its delivery becomes seen then.
func (r *router) onSeen(sessionID string, p model.MessageSeen, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(p.MessageID)
	if !ok || cur.ToSession != sessionID || !cur.SeenAt.IsZero() {
		return
	}
	switch cur.State {
	case model.MsgDelivered:
		cur.State = model.MsgSeen
	case model.MsgPending, model.MsgAnswered:
	default:
		return // failed, expired or cancelled
	}
	cur.SeenAt = nodeTime(at, cur.CreatedAt)
	cur.SeenTurn, cur.SeenItem = clipText(p.TurnID, 200), clipText(p.ItemID, 200)
	if cur.DeliveredAt.IsZero() {
		cur.DeliveredAt = cur.SeenAt
	}
	_ = r.put(cur)
}

// nodeTime bounds a time a node reported to between since and now, so a
// node's clock cannot put a receipt before its message or in the future.
func nodeTime(at, since time.Time) time.Time {
	now := time.Now().UTC()
	switch {
	case at.After(now):
		return now
	case at.Before(since):
		return since
	}
	return at.UTC()
}

// settle moves a still-pending message on, reading and writing it under
// r.mu (as reply does), so a reply or another transition that landed
// meanwhile is never overwritten by a stale copy. It returns the current
// record and whether it changed it.
func (r *router) settle(id string, fn func(*model.Message)) (model.Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || cur.State != model.MsgPending {
		return cur, false
	}
	fn(&cur)
	_ = r.put(cur)
	return cur, true
}

// promoted records the mode a promoted message went with.
func (r *router) promoted(id, mode string) model.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if ok && mode != "" && cur.Mode != mode {
		cur.Mode = mode
		_ = r.put(cur)
	}
	return cur
}

// cancelPending cancels a message the coordinator still holds, unless a
// delivery of it is under way (the node may be taking it).
func (r *router) cancelPending(id, by string) error {
	r.mu.Lock()
	if r.inflight[id] {
		r.mu.Unlock()
		return errors.New("the message is being delivered now; try again")
	}
	r.mu.Unlock()
	if !r.cancelled(id, by) {
		return errors.New("the message is no longer pending")
	}
	return nil
}

// cancelled marks a pending or queued message cancelled, wakes a fleet_ask
// waiting on it (it will not be answered) and tells an agent that sent it.
// It reports whether it changed the message.
func (r *router) cancelled(id, by string) bool {
	r.mu.Lock()
	cur, ok := r.s.store.Message(id)
	if !ok || (cur.State != model.MsgPending && cur.State != model.MsgDelivered) || r.inflight[id] && cur.State == model.MsgPending {
		r.mu.Unlock()
		return false
	}
	cur.State, cur.Error = model.MsgCancelled, "cancelled by "+by
	_ = r.put(cur)
	ws := r.waiters[id]
	delete(r.waiters, id)
	for _, w := range ws {
		w <- cur
	}
	r.mu.Unlock()
	r.notifySender(cur, fmt.Sprintf("Your %s message %s to %s was cancelled by %s before it was delivered.", cur.Kind, cur.ID, cur.To, by))
	return true
}

func (r *router) fail(m model.Message, why string) model.Message {
	out, changed := r.settle(m.ID, func(cur *model.Message) { cur.State, cur.Error = model.MsgFailed, why })
	if !changed {
		if _, stored := r.s.store.Message(m.ID); stored {
			return out
		}
		// Not stored yet (a reply to an asker that is gone): record it.
		m.State, m.Error = model.MsgFailed, why
		_ = r.put(m)
		out = m
	}
	r.notifySender(out, fmt.Sprintf("Your %s message %s to %s could not be delivered: %s", out.Kind, out.ID, out.To, why))
	return out
}

// expire expires a message still waiting at the coordinator for its host.
func (r *router) expire(m model.Message) {
	r.expireIn(m.ID, false, "expired before it could be delivered", "its host was offline")
}

// onExpired expires a message its target's node dropped from the session's
// queue past its expiry (message.expired), and tells the sender.
func (r *router) onExpired(sessionID, messageID string) {
	m, ok := r.s.store.Message(messageID)
	if !ok || m.ToSession != sessionID {
		return // not this session's to expire
	}
	r.expireQueued(m.ID)
}

// expireQueued expires a message the node dropped from its queue.
func (r *router) expireQueued(id string) {
	r.expireIn(id, true, "expired in the session's queue before it was delivered", "it waited in the session's queue past its expiry")
}

// expireIn marks a pending message (with queued, also one its node had
// accepted) expired, wakes a fleet_ask waiting on it and tells an agent
// that sent it, with the notice a coordinator-side expiry sends.
func (r *router) expireIn(id string, queued bool, why, cause string) {
	r.mu.Lock()
	cur, ok := r.s.store.Message(id)
	if !ok || !(cur.State == model.MsgPending || queued && cur.State == model.MsgDelivered) {
		r.mu.Unlock()
		return
	}
	if !queued && r.inflight[id] {
		// Its node may be taking it now: the loop looks again once that
		// delivery has settled (delivered, or still pending and expired).
		r.mu.Unlock()
		return
	}
	cur.State, cur.Error = model.MsgExpired, why
	_ = r.put(cur)
	ws := r.waiters[id]
	delete(r.waiters, id)
	for _, w := range ws {
		w <- cur // the ask itself, expired
	}
	r.mu.Unlock()
	r.notifySender(cur, fmt.Sprintf("Your %s message %s to %s expired before it could be delivered (%s).", cur.Kind, cur.ID, cur.To, cause))
}

// notifySender queues a system notice for the sending session. Notices are
// never sent about notices, completion messages or replies.
func (r *router) notifySender(m model.Message, text string) {
	if m.FromSession == "" || m.Kind == model.MsgSystem || m.Kind == model.MsgDone || m.Kind == model.MsgReply {
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
		r.deliverLater(n)
	}
}

// onFailed fails a message its target's node accepted but could not hand
// to the harness (message.failed), and tells the sender.
func (r *router) onFailed(sessionID, messageID, why string) {
	m, ok := r.s.store.Message(messageID)
	if !ok || m.ToSession != sessionID {
		return // not this session's to fail
	}
	why = clipText(why, 500)
	if why == "" {
		why = "the harness did not take it"
	}
	r.failAccepted(m.ID, why, func(m model.Message) string {
		return fmt.Sprintf("Your %s message %s to %s could not be delivered: %s", m.Kind, m.ID, m.To, why)
	})
}

// failAsksTo fails the asks a session was given and will not answer, now
// that it has stopped or gone, and tells their senders.
func (r *router) failAsksTo(sess model.Session, why string) {
	for _, m := range r.s.store.Messages(store.MessageFilter{Session: sess.ID}) {
		if m.Kind != model.MsgAsk || m.ToSession != sess.ID || !m.Accepted() {
			continue
		}
		r.failAccepted(m.ID, why, func(m model.Message) string {
			return fmt.Sprintf("Your ask %s to %s will not be answered: %s.", m.ID, m.To, why)
		})
	}
}

// failOrphanedAsks fails the unanswered asks of sessions that ended before
// asks were failed when their session ended.
func (r *router) failOrphanedAsks() {
	ended := map[string]model.Session{}
	for _, sess := range r.s.store.Sessions() {
		if !live(sess) {
			ended[sess.ID] = sess
		}
	}
	seen := map[string]bool{}
	for _, m := range r.s.store.Messages(store.MessageFilter{}) {
		if sess, ok := ended[m.ToSession]; ok && !seen[sess.ID] && m.Kind == model.MsgAsk && m.Accepted() {
			seen[sess.ID] = true
			r.failAsksTo(sess, "its session "+sess.Status)
		}
	}
}

// failAccepted fails a message that is pending or was delivered (or seen,
// but not answered), wakes a fleet_ask waiting on it, and notifies its
// sender.
func (r *router) failAccepted(id, why string, notice func(model.Message) string) {
	r.mu.Lock()
	cur, ok := r.s.store.Message(id)
	if !ok || (cur.State != model.MsgPending && !cur.Accepted()) {
		r.mu.Unlock()
		return
	}
	cur.State, cur.Error = model.MsgFailed, why
	_ = r.put(cur)
	ws := r.waiters[id]
	delete(r.waiters, id)
	for _, w := range ws {
		w <- cur // the ask itself, failed
	}
	r.mu.Unlock()
	r.notifySender(cur, notice(cur))
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
	msgs, err := r.send(ctx, from, model.MsgAsk, fleet.SendArgs{To: args.To, Text: args.Text, TTLSeconds: args.TTLSeconds,
		Images: args.Images, Attachments: args.Attachments, Files: args.Files, FileAttachments: args.FileAttachments}, opts...)
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
			if rep.ID == m.ID {
				res.Status, res.Error = fleet.AskFailed, rep.Error
				return res
			}
			res.Status, res.Reply, res.ReplyFrom = fleet.AskReplied, rep.Text, rep.From
			return res
		}
	}
	res.Note = "No reply yet. It will arrive later as a message in this session; or call fleet_inbox with ticket=" + m.ID + "."
	return res
}

// waitReply blocks until askID is answered, d elapses or ctx ends. It
// returns the reply, or the ask itself if it failed.
func (r *router) waitReply(ctx context.Context, askID string, d time.Duration) *model.Message {
	r.mu.Lock()
	if ask, ok := r.s.store.Message(askID); ok && (ask.State == model.MsgFailed || ask.State == model.MsgCancelled) {
		r.mu.Unlock()
		return &ask
	}
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
	rep := model.Message{ID: ulid.New(), Kind: model.MsgReply, From: from.addr(), FromSession: from.sessionID(), Via: from.via, Owner: from.owner, ViaToken: from.token,
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
	if cur, ok := r.s.store.Message(ask.ID); ok {
		switch {
		case cur.ReplyID != "":
			r.mu.Unlock()
			return fleet.ReplyResult{}, errors.New("this ask has already been answered")
		case cur.State == model.MsgCancelled, cur.State == model.MsgExpired, cur.State == model.MsgFailed:
			// Its sender was told it will not be answered (and a waiting
			// fleet_ask has returned): an answer now would contradict that.
			r.mu.Unlock()
			return fleet.ReplyResult{}, fmt.Errorf("this ask is %s and can no longer be answered", cur.State)
		}
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
		case model.MsgFailed, model.MsgExpired, model.MsgCancelled:
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
			switch {
			case rep != nil && rep.ID == ask.ID:
				t.Status, t.Error = fleet.AskFailed, rep.Error
			case rep != nil:
				t.Status, t.Reply, t.ReplyFrom = fleet.AskReplied, rep.Text, rep.From
			}
		}
		res.Ticket = t
		// Checking an ask reads nothing else: swk mcp polls a ticket while
		// fleet_ask waits, and would mark messages read unseen.
		res.Messages = []fleet.InboxMessage{}
		return res, nil
	}
	limit := args.Limit
	if limit <= 0 || limit > fleet.MaxInboxLimit {
		limit = fleet.DefaultInboxLimit
	}
	var msgs []model.Message
	switch {
	case from.sess == nil:
		msgs = r.s.store.Messages(store.MessageFilter{ToUser: true, Limit: limit})
	case args.All:
		// What it sent, and what reached it: a message to it that never
		// went into its input (queued, cancelled, expired, failed) is not
		// its to read.
		for _, m := range r.s.store.Messages(store.MessageFilter{Session: from.sess.ID}) {
			if len(msgs) == limit {
				break
			}
			if m.ToSession == from.sess.ID && m.FromSession != from.sess.ID && !m.Entered() {
				continue
			}
			msgs = append(msgs, m)
		}
	default:
		msgs, res.More = r.unread(from.sess.ID, limit)
	}
	for i := range msgs {
		msgs[i].Text = clipInbox(msgs[i].Text)
	}
	if args.Verbose {
		if msgs == nil {
			msgs = []model.Message{}
		}
		res.Messages = msgs
		return res, nil
	}
	out := make([]fleet.InboxMessage, len(msgs))
	for i, m := range msgs {
		out[i] = fleet.InboxMessage{ID: m.ID, Kind: m.Kind, From: m.From, CreatedAt: m.CreatedAt, Text: m.Text, Re: m.ReplyTo}
		if from.sess != nil && m.FromSession == from.sess.ID {
			out[i].To, out[i].State, out[i].DeliveredAt, out[i].SeenAt = m.To, m.State, m.DeliveredAt, m.SeenAt
		}
	}
	res.Messages = out
	return res, nil
}

// unread returns the oldest limit messages to a session after its inbox
// cursor, oldest first, with how many more there are, and moves the cursor
// past those it returns. Only messages that went into the session's input
// are unread: not ones still pending (in flight, or a reply held for its
// asker) or waiting in its queue (they arrive as a turn), not ones
// cancelled, expired or failed before they reached it, and not replies
// handed straight to a waiting fleet_ask (the session saw them there).
func (r *router) unread(sessionID string, limit int) ([]model.Message, int) {
	sess, ok := r.s.store.Session(sessionID)
	if !ok {
		return nil, 0
	}
	var out []model.Message
	for _, m := range r.s.store.Messages(store.MessageFilter{Session: sessionID}) {
		if m.ID <= sess.InboxRead {
			break // newest first
		}
		if m.ToSession != sessionID || !m.Entered() || m.Inline {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	more := 0
	if len(out) > limit {
		more = len(out) - limit
		out = out[:limit]
	}
	if len(out) > 0 {
		r.markRead(sessionID, out[len(out)-1].ID)
	}
	return out, more
}

// markRead moves a session's inbox cursor forward to id.
func (r *router) markRead(sessionID, id string) {
	_, _ = r.s.store.UpdateSession(sessionID, func(s *model.Session) {
		if id > s.InboxRead {
			s.InboxRead = id
		}
	})
}

// clipInbox cuts a message's text for fleet_inbox, saying how much it left
// out.
func clipInbox(text string) string {
	if len(text) <= fleet.InboxTextBytes {
		return text
	}
	cut := fleet.InboxTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… [%d more bytes cut]", text[:cut], len(text)-cut)
}

// onDelivered records the hops of a message that went into a session's
// input (message.delivered, with the mode the node reports). A turn takes
// the deepest hop count of everything it took in: the node reports each
// message of a batch, and each steer into the running turn, one by one,
// and none of them may lower the count. Only a message that starts a turn
// after the last one ended (an idle session) starts it over.
func (r *router) onDelivered(sessionID, messageID, mode string) {
	m, ok := r.s.store.Message(messageID)
	if !ok || m.ToSession != sessionID {
		// Not a routed message to this session (a person's spawn prompt,
		// recorded by started, or another session's message): keep what
		// started the work, so a node cannot reset hops by reporting
		// made-up or borrowed messages.
		return
	}
	// The session has seen it: fleet_inbox need not list it as unread.
	defer r.markRead(sessionID, m.ID)
	if !m.SeenAt.IsZero() {
		// Its turn is over already (a late or repeated report): it starts
		// nothing.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tr, ok := r.inbound[sessionID]
	switch {
	case !ok || tr.idle && mode != model.ModeSteer:
		tr = trigger{hops: m.Hops}
	case contains(tr.msgs, m.ID):
		return // reported again (a delivery retried after a busy harness)
	default:
		tr.hops = max(tr.hops, m.Hops)
	}
	if len(tr.msgs) < maxTriggerMsgs {
		tr.msgs = append(tr.msgs, m.ID)
	}
	r.inbound[sessionID] = tr
}

// rebuildTriggers recovers what started each live session's current work
// from its stored messages, so a restart does not hand running chains
// fresh hop counts: the messages it took in since its last completed turn
// (the current turn's), or failing those the last completed turn's.
func (r *router) rebuildTriggers() {
	for _, sess := range r.s.store.Sessions() {
		if !live(sess) {
			continue
		}
		var lastSeen time.Time
		var lastTurn string
		var entered []model.Message
		for _, m := range r.s.store.Messages(store.MessageFilter{Session: sess.ID}) {
			if m.ToSession != sess.ID || !m.Entered() {
				continue
			}
			entered = append(entered, m)
			if m.SeenAt.After(lastSeen) {
				lastSeen, lastTurn = m.SeenAt, m.SeenTurn
			}
		}
		sort.Slice(entered, func(i, j int) bool { return entered[i].ID < entered[j].ID })
		var cur, prev trigger
		for _, m := range entered {
			switch {
			case m.SeenAt.IsZero() && !m.DeliveredAt.Before(lastSeen):
				cur.hops = max(cur.hops, m.Hops)
				cur.msgs = append(cur.msgs, m.ID)
			case !m.SeenAt.IsZero() && (lastTurn != "" && m.SeenTurn == lastTurn || lastTurn == "" && m.SeenAt.Equal(lastSeen)):
				prev.hops = max(prev.hops, m.Hops)
				prev.msgs = append(prev.msgs, m.ID)
			}
		}
		tr := cur
		if len(tr.msgs) == 0 {
			tr = prev
		}
		if len(tr.msgs) == 0 {
			continue
		}
		if len(tr.msgs) > maxTriggerMsgs {
			tr.msgs = tr.msgs[len(tr.msgs)-maxTriggerMsgs:]
		}
		tr.idle = sess.Status == model.StatusIdle
		r.mu.Lock()
		if old, ok := r.inbound[sess.ID]; !ok || old.hops < tr.hops {
			r.inbound[sess.ID] = tr
		}
		r.mu.Unlock()
	}
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
	for _, id := range tr.msgs {
		if m, ok := r.s.store.Message(id); ok && m.Kind == model.MsgAsk && m.FromSession == parent.ID && m.State == model.MsgAnswered {
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
		r.deliverLater(m)
	}
}
