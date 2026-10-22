// SPDX-License-Identifier: GPL-3.0-or-later

// Package node is `swk node`: it runs harness sessions on one workstation,
// numbers and spools their events, keeps a link to the coordinator, and
// executes the coordinator's commands.
//
// Durability: every event is appended to spool/<session>.jsonl before it is
// sent. The coordinator acknowledges the highest seq it has stored; acked
// events leave memory immediately and the spool file is compacted once it is
// fully acked and large, after state.json records the acked seq. After a
// restart or reconnect the node resends everything above the coordinator's
// acked seq, and the coordinator drops duplicates, so events are delivered
// at least once and stored exactly once.
package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/adapter/agyad"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/harness"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/version"
	"github.com/jm2/switchworks/internal/wire"
)

// Config is node.json.
type Config struct {
	Host            string `json:"host"`
	CoordinatorAPI  string `json:"coordinator_api"`
	CoordinatorNode string `json:"coordinator_node"`

	Codex struct {
		Bin  string `json:"bin,omitempty"`
		Home string `json:"home,omitempty"` // CODEX_HOME override
		// Args is codex's whole argument list, subcommand included
		// (default ["app-server"]); the node refuses one without a
		// subcommand. Config overrides belong in Config.
		Args []string `json:"args,omitempty"`
		// Config holds Codex config overrides, passed as -c key=value
		// ahead of Args: values in TOML, nested objects as dotted keys.
		Config map[string]json.RawMessage `json:"config,omitempty"`
		// Lean adds leanCodexConfig (a smaller tool set and an earlier
		// compaction) to Config, for the keys Config and Args leave unset.
		Lean bool `json:"lean,omitempty"`
		// Permissions names a permission profile in codex's config.toml
		// for workspace-write sessions, e.g. one that turns the network on
		// but denies reads of credentials. Without it they get codex's
		// legacy workspace-write sandbox, which ignores profiles.
		Permissions string `json:"permissions,omitempty"`
	} `json:"codex"`
	Agy struct {
		Bin  string   `json:"bin,omitempty"`
		Args []string `json:"args,omitempty"`
		agyad.Options
	} `json:"agy"`

	// ACP lists Agent Client Protocol agents by harness name. "goose" is
	// built in (`goose acp`, used when goose is on PATH); an entry for it
	// overrides its binary, args, env or mode mapping, or disables it.
	// Other names add agents, e.g. "opencode": {"bin": "opencode",
	// "args": ["acp"]}.
	ACP map[string]harness.ACPAgent `json:"acp,omitempty"`

	// Defaults applied to spawns that do not set them.
	DefaultApprovalPolicy string `json:"default_approval_policy,omitempty"` // codex: never | on-request | untrusted
	DefaultSandbox        string `json:"default_sandbox,omitempty"`         // read-only | workspace-write | danger-full-access

	// MCP controls the fleet tools attached to each session (Codex and ACP
	// agents per session; agy through its workspace config, opt-in).
	MCP struct {
		Disabled bool     `json:"disabled,omitempty"`
		Command  string   `json:"command,omitempty"` // default: this swk binary
		Args     []string `json:"args,omitempty"`    // default: ["mcp"]
	} `json:"mcp"`
}

// spool compaction threshold.
const compactBytes = 1 << 20

type queuedMsg struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Via     string `json:"via,omitempty"`   // a person's credential
	Owner   bool   `json:"owner,omitempty"` // it is the owner's: no header
	Text    string `json:"text"`
	Kind    string `json:"kind,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
	// Attachments: metadata only. The bytes wait in the session's
	// attachment directory (see images.go) until the message is delivered.
	Attachments []model.Attachment `json:"attachments,omitempty"`
}

// seenCap bounds the per-session list of delivered message ids kept for
// deduplication of coordinator redeliveries.
const seenCap = 512

// record is a session's persisted state.
type record struct {
	ID        string            `json:"id"`
	Path      string            `json:"path"`
	Harness   string            `json:"harness"`
	Spec      adapter.StartSpec `json:"spec"`
	NativeRef *model.NativeRef  `json:"native_ref,omitempty"`
	Status    string            `json:"status"`
	AckedSeq  int64             `json:"acked_seq"`
	Queue     []queuedMsg       `json:"queue,omitempty"`
	Seen      []string          `json:"seen,omitempty"` // recently accepted message ids
	Stopped   bool              `json:"stopped,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type session struct {
	n *Node

	// policyMu orders approval policy changes, so the harness, the record
	// and the session.policy events agree on the last one.
	policyMu sync.Mutex

	mu         sync.Mutex
	rec        record
	localReqs  int // local API requests in progress (local.go)
	h          adapter.Handle
	seq        int64         // last assigned seq
	unacked    []model.Event // seq > rec.AckedSeq
	sentSeq    int64         // last seq sent on the current connection
	spool      *os.File
	spoolSize  int64
	dirtySpool bool
	delta      *model.ItemDelta // coalescing buffer
	deltaSince time.Time
	delivering bool
	imgMu      sync.Mutex // orders storing queued images against gcImages

	// Stall recovery (see tryDeliver and sweepQueues): redeliver is set
	// when a trigger to deliver arrives during a delivery, busyTries counts
	// ErrBusy answers in a row while the session is idle, and stuckSince is
	// when a sweep first saw the session idle with a queue.
	redeliver  bool
	busyTries  int
	stuckSince time.Time

	reportedQueue int // queue length last reported in session.queue

	// Steers in flight: steering is set while one is being delivered, and
	// steerBuf holds the messages that arrived meanwhile, to go in together.
	steering bool
	steerBuf []*steerReq

	// Settled detection: busy is set when a turn starts and cleared once the
	// session is idle with an empty queue, at which point session.settled is
	// emitted with the last turn's outcome.
	activeTurn string
	busy       bool
	lastTurn   string
	lastStatus string
	lastText   string
}

// Node is a running node.
type Node struct {
	dir   string
	cfg   Config
	logf  func(string, ...any)
	hopts harness.Options

	mu       sync.Mutex
	adapters map[string]adapter.Adapter
	sessions map[string]*session

	connMu sync.Mutex
	conn   *wire.Conn
	wake   chan struct{}

	reqMu   sync.Mutex
	reqs    map[string]chan reqReply // node -> coordinator requests in flight
	local   *localAPI
	mcpCmd  string
	mcpArgs []string

	stateMu sync.Mutex
	closing chan struct{}
	wg      sync.WaitGroup

	status *runstatus.Reporter // status.json for `swk service status` and the tray
}

// New loads node state from dir and resumes live sessions.
func New(dir string, cfg Config, hopts harness.Options, logf func(string, ...any)) (*Node, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		return nil, err
	}
	n := &Node{dir: dir, cfg: cfg, logf: logf, hopts: hopts,
		adapters: map[string]adapter.Adapter{}, sessions: map[string]*session{},
		wake: make(chan struct{}, 1), closing: make(chan struct{}), reqs: map[string]chan reqReply{}}
	if !cfg.MCP.Disabled {
		if err := n.startLocal(); err != nil {
			logf("fleet tools disabled: local API: %v", err)
		}
	}
	recs, err := n.loadState()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		s := &session{n: n, rec: rec}
		if err := s.openSpool(); err != nil {
			return nil, fmt.Errorf("session %s spool: %w", rec.ID, err)
		}
		n.sessions[rec.ID] = s
	}
	n.status = runstatus.Start(dir, runstatus.Status{Role: "node", Host: cfg.Host,
		Coordinator: cfg.CoordinatorNode, WebUI: webUIFor(cfg.CoordinatorAPI)})
	n.wg.Add(2)
	go n.ticker()
	go n.sender()
	for _, s := range n.sessionList() {
		if !s.rec.Stopped {
			n.resume(s)
		}
	}
	return n, nil
}

// webUIFor turns the coordinator API address from node.json into the web
// UI's URL. A coordinator behind `tailscale serve` has a different public
// URL; its own status file reports that one.
func webUIFor(api string) string {
	if api == "" {
		return ""
	}
	return "https://" + api + "/"
}

func (n *Node) sessionList() []*session {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*session, 0, len(n.sessions))
	for _, s := range n.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rec.ID < out[j].rec.ID })
	return out
}

// Adapters lists the harnesses available on this host.
func (n *Node) Adapters() []string { return harness.Available(n.hopts) }

func (n *Node) adapterFor(name string) (adapter.Adapter, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if a, ok := n.adapters[name]; ok {
		return a, nil
	}
	a, err := harness.New(name, n.hopts)
	if err != nil {
		return nil, err
	}
	n.adapters[name] = a
	return a, nil
}

// --- persistence ---

func (n *Node) statePath() string { return filepath.Join(n.dir, "state.json") }

func (n *Node) loadState() ([]record, error) {
	b, err := os.ReadFile(n.statePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st struct {
		Sessions []record `json:"sessions"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json: %w", err)
	}
	return st.Sessions, nil
}

func (n *Node) saveState() error {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	var st struct {
		Sessions []record `json:"sessions"`
	}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		st.Sessions = append(st.Sessions, s.rec)
		s.mu.Unlock()
	}
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := n.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, n.statePath())
}

func (s *session) spoolPath() string { return filepath.Join(s.n.dir, "spool", s.rec.ID+".jsonl") }

// openSpool loads unacked events and recovers the last seq.
func (s *session) openSpool() error {
	f, err := os.OpenFile(s.spoolPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	s.seq = s.rec.AckedSeq
	br := bufio.NewReaderSize(f, 1<<20)
	var off int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var ev model.Event
			if json.Unmarshal(line, &ev) == nil {
				if ev.Seq > s.seq {
					s.seq = ev.Seq
				}
				if ev.Seq > s.rec.AckedSeq {
					s.unacked = append(s.unacked, ev)
				}
			}
			off += int64(len(line))
		}
		if err != nil {
			break
		}
	}
	if err := f.Truncate(off); err != nil { // drop a torn final line
		f.Close()
		return err
	}
	if _, err := f.Seek(off, 0); err != nil {
		f.Close()
		return err
	}
	s.spool, s.spoolSize = f, off
	return nil
}

// --- events ---

func (s *session) sink(ev adapter.Event) {
	s.mu.Lock()
	if ev.Kind == model.KindItemDelta {
		if d, ok := ev.Payload.(model.ItemDelta); ok {
			if s.delta != nil && s.delta.ItemID == d.ItemID && s.delta.TurnID == d.TurnID {
				s.delta.Text += d.Text
			} else {
				s.flushDeltaLocked()
				cp := d
				s.delta, s.deltaSince = &cp, time.Now()
			}
			s.mu.Unlock()
			return
		}
	}
	s.flushDeltaLocked()
	s.appendLocked(ev.Kind, ev.Payload)
	idle := false
	save := false
	switch ev.Kind {
	case model.KindTurnStarted:
		if p, ok := ev.Payload.(model.TurnStarted); ok {
			s.activeTurn, s.busy = p.TurnID, true
		}
	case model.KindTurnCompleted:
		if p, ok := ev.Payload.(model.TurnCompleted); ok {
			if s.activeTurn == p.TurnID {
				s.activeTurn = ""
			}
			s.lastTurn, s.lastStatus = p.TurnID, p.Status
			idle = true // re-check delivery and settling now that the turn is done
		}
	case model.KindItemCompleted:
		if p, ok := ev.Payload.(model.Item); ok && p.ItemType == "agentMessage" && p.Text != "" {
			s.lastText = p.Text
		}
	case model.KindSessionRegistered:
		if p, ok := ev.Payload.(model.SessionRegistered); ok {
			ref := p.NativeRef
			s.rec.NativeRef = &ref
			save = true
		}
	case model.KindSessionStatus:
		if p, ok := ev.Payload.(model.SessionStatus); ok {
			s.rec.Status = p.Status
			idle = p.Status == model.StatusIdle
		}
	}
	s.mu.Unlock()
	if save {
		_ = s.n.saveState()
	}
	if idle {
		go s.n.tryDeliver(s)
	}
	s.n.wakeSender()
}

func (s *session) flushDeltaLocked() {
	if s.delta == nil {
		return
	}
	d := *s.delta
	s.delta = nil
	s.appendLocked(model.KindItemDelta, d)
}

// emit records a node-originated event.
func (s *session) emit(kind string, payload any) {
	s.mu.Lock()
	s.flushDeltaLocked()
	s.appendLocked(kind, payload)
	s.mu.Unlock()
	s.n.wakeSender()
}

// Size limits on what a node sends: one event's payload, and one batch of
// events (the coordinator refuses lines over wire.MaxLine, which would
// otherwise disconnect the node over the same batch forever).
const (
	maxEventPayload = 4 << 20
	maxBatchBytes   = 8 << 20
)

// boundedPayload encodes payload, shrinking an oversized one: a turn diff
// keeps its first part, anything else is replaced by a note of its size.
func boundedPayload(kind string, payload any) json.RawMessage {
	b := model.Marshal(payload)
	if len(b) <= maxEventPayload {
		return b
	}
	// The size that matters is the encoded one, which can be several times
	// the raw text (JSON escapes <, >, & and control bytes as \u00XX).
	if d, ok := payload.(model.TurnDiff); ok {
		full := len(d.Diff)
		for cut := min(1<<20, full); cut > 0; cut /= 2 {
			c := cut // back up to a character boundary
			for c > 0 && c < full && !utf8.RuneStart(d.Diff[c]) {
				c--
			}
			t := d
			t.Diff = d.Diff[:c] + fmt.Sprintf("\n… (diff truncated by Switchworks: %d bytes in all)\n", full)
			if tb := model.Marshal(t); len(tb) <= maxEventPayload {
				return tb
			}
		}
	}
	return model.Marshal(map[string]any{"truncated": true, "kind": kind, "bytes": len(b)})
}

func (s *session) appendLocked(kind string, payload any) {
	s.seq++
	ev := model.Event{ID: ulid.New(), SessionID: s.rec.ID, Seq: s.seq, TS: time.Now().UTC(), Kind: kind,
		Source: s.rec.Harness, Payload: boundedPayload(kind, payload)}
	b, _ := json.Marshal(ev)
	b = append(b, '\n')
	if s.spool != nil {
		if n, err := s.spool.Write(b); err == nil {
			s.spoolSize += int64(n)
			s.dirtySpool = true
		} else {
			s.n.logf("session %s: spool write: %v", s.rec.Path, err)
		}
	}
	s.unacked = append(s.unacked, ev)
}

func (n *Node) wakeSender() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// ticker flushes coalesced deltas, fsyncs spools and sweeps for stalled
// queues.
func (n *Node) ticker() {
	defer n.wg.Done()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	syncEvery := 0
	lastSweep := time.Now()
	for {
		select {
		case <-t.C:
		case <-n.closing:
			return
		}
		if now := time.Now(); now.Sub(lastSweep) >= sweepEvery {
			lastSweep = now
			n.sweepQueues(now)
		}
		syncEvery++
		flushed := false
		for _, s := range n.sessionList() {
			s.mu.Lock()
			if s.delta != nil && time.Since(s.deltaSince) >= 250*time.Millisecond {
				s.flushDeltaLocked()
				flushed = true
			}
			if syncEvery%10 == 0 && s.dirtySpool && s.spool != nil {
				_ = s.spool.Sync()
				s.dirtySpool = false
			}
			s.mu.Unlock()
		}
		if flushed {
			n.wakeSender()
		}
	}
}

// --- sessions ---

func (n *Node) resume(s *session) {
	s.mu.Lock()
	rec := s.rec
	s.mu.Unlock()
	a, err := n.adapterFor(rec.Harness)
	if err != nil {
		s.emit(model.KindError, model.ErrorEvent{Message: "resume: " + err.Error()})
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusError, Detail: err.Error()})
		return
	}
	if rec.NativeRef == nil {
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusError, Detail: "node restarted before the harness assigned an id"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	spec := rec.Spec
	spec.MCP = n.mcpFor(rec.ID, rec.Path)
	// No turn survives a node restart, so no harness still needs the
	// image files of one.
	_ = os.RemoveAll(n.scratchDir(rec.ID))
	spec.ScratchDir = n.scratchDir(rec.ID)
	h, err := a.Resume(ctx, *rec.NativeRef, spec, s.sink)
	if err != nil {
		n.logf("session %s: resume failed: %v", rec.Path, err)
		s.emit(model.KindError, model.ErrorEvent{Message: "resume: " + err.Error()})
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusError, Detail: "resume failed"})
		return
	}
	s.mu.Lock()
	s.h = h
	s.reportQueueLocked() // a backlog kept across a restart
	s.mu.Unlock()
	n.wakeSender()
	n.logf("session %s resumed (%s %s)", rec.Path, rec.Harness, rec.NativeRef.ID)
	go n.tryDeliver(s)
}

func (n *Node) spawn(ctx context.Context, cmd *wire.SpawnCmd) (json.RawMessage, error) {
	if cmd == nil {
		return nil, errors.New("spawn: missing body")
	}
	if !ulid.Valid(cmd.Session.ID) {
		return nil, errors.New("spawn: bad session id")
	}
	n.mu.Lock()
	if existing, ok := n.sessions[cmd.Session.ID]; ok {
		n.mu.Unlock()
		existing.mu.Lock()
		ref, sp := existing.rec.NativeRef, existing.rec.Spec
		existing.mu.Unlock()
		return spawnResult(ref, sp), nil // idempotent retry
	}
	n.mu.Unlock()
	spec := cmd.Spec
	harnessName := spec.Harness
	if harnessName == "" {
		harnessName = "codex"
	}
	a, err := n.adapterFor(harnessName)
	if err != nil {
		return nil, err
	}
	approval, sandbox := spec.ApprovalPolicy, spec.Sandbox
	if approval == "" {
		approval = n.cfg.DefaultApprovalPolicy
	}
	if sandbox == "" {
		sandbox = n.cfg.DefaultSandbox
	}
	if spec.Cwd, err = expandHome(spec.Cwd); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(spec.Cwd); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory on %s", spec.Cwd, n.cfg.Host)
	}
	images, err := attach.Normalize(spec.Attachments)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 && !harness.TakesImages(harnessName) {
		return nil, noImages(harnessName)
	}
	if len(images) > 0 && strings.TrimSpace(spec.Prompt) == "" {
		return nil, errors.New("images need a prompt to go with them")
	}
	if spec.CwdRoot != "" || spec.ByAgent {
		// An agent's spawn: the coordinator checked the path's text; here
		// symlinks are resolved too, so a link inside the root cannot lead
		// out of it.
		if err := proc.CheckWithin(spec.CwdRoot, spec.Cwd); err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(spec.Cwd)
		if err != nil {
			return nil, fmt.Errorf("cwd %q: %w", spec.Cwd, err)
		}
		if spec.NotBelow != "" {
			// The parent can write in its own cwd, so anything below it
			// could be re-pointed once checked.
			nb, err := filepath.EvalSymlinks(spec.NotBelow)
			if err != nil || (real != nb && proc.CheckWithin(nb, real) == nil) {
				return nil, fmt.Errorf("cwd %q is inside %q, which the spawning agent can write; it may spawn only in %q itself",
					spec.Cwd, spec.NotBelow, spec.NotBelow)
			}
		}
		// Keep the resolved path, and pin it: adapters check before each
		// use that it still resolves to itself (proc.CheckPinned), so a
		// directory on it swapped for a symlink later is not followed.
		spec.Cwd = real
		if spec.CwdRoot == "" {
			spec.CwdRoot = real // any directory was allowed: pin this one
		} else if r, err := filepath.EvalSymlinks(spec.CwdRoot); err == nil {
			spec.CwdRoot = r // resolved once, like the cwd
		}
	}
	s := &session{n: n, rec: record{
		ID: cmd.Session.ID, Path: cmd.Session.Path, Harness: harnessName, Status: model.StatusStarting, CreatedAt: time.Now(),
		Spec: adapter.StartSpec{Name: cmd.Session.Path, Cwd: spec.Cwd, Model: spec.Model, Provider: spec.Provider,
			ApprovalPolicy: approval, Sandbox: sandbox, CwdRoot: spec.CwdRoot, ByAgent: spec.ByAgent || spec.CwdRoot != ""},
	}}
	if err := s.openSpool(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.sessions[s.rec.ID] = s
	n.mu.Unlock()
	startSpec := s.rec.Spec
	startSpec.MCP = n.mcpFor(s.rec.ID, s.rec.Path)
	startSpec.ScratchDir = n.scratchDir(s.rec.ID)
	h, err := a.Start(ctx, startSpec, s.sink)
	failStart := func(err error) (json.RawMessage, error) {
		// The coordinator forgets a session whose spawn fails, so
		// nothing of it is kept here either: no record, spool or images
		// (which would otherwise be resent, and adopted, forever).
		n.mu.Lock()
		delete(n.sessions, s.rec.ID)
		n.mu.Unlock()
		s.mu.Lock()
		s.rec.Stopped = true
		s.unacked = nil
		if s.spool != nil {
			_ = s.spool.Close()
			s.spool = nil
		}
		s.mu.Unlock()
		_ = os.Remove(s.spoolPath())
		_ = n.saveState()
		n.dropImages(s.rec.ID)
		return nil, err
	}
	if err != nil {
		return failStart(err)
	}
	var metas []model.Attachment
	if len(images) > 0 {
		// Only now is it known whether an ACP agent takes images.
		if !h.Caps().Images {
			_ = h.Stop(ctx)
			return failStart(noImages(harnessName))
		}
		if metas, err = n.storeImages(s.rec.ID, images); err != nil {
			_ = h.Stop(ctx)
			return failStart(err)
		}
	}
	s.mu.Lock()
	s.h = h
	if strings.TrimSpace(spec.Prompt) != "" {
		s.rec.Queue = append(s.rec.Queue, queuedMsg{ID: ulid.New(), From: "user:spawn", Via: cmd.PromptVia, Owner: cmd.PromptOwner,
			Text: spec.Prompt, Attachments: metas})
	}
	ref, sp := s.rec.NativeRef, s.rec.Spec
	s.mu.Unlock()
	_ = n.saveState()
	n.logf("session %s started (%s)", s.rec.Path, harnessName)
	go n.tryDeliver(s)
	return spawnResult(ref, sp), nil
}

// spawnResult answers a spawn with the session's native reference and the
// approval policy and sandbox it actually runs with (after node defaults),
// which the coordinator gives to sessions it spawns.
func spawnResult(ref *model.NativeRef, sp adapter.StartSpec) json.RawMessage {
	return model.Marshal(wire.SpawnResult{NativeRef: ref, ApprovalPolicy: sp.ApprovalPolicy, Sandbox: sp.Sandbox, Cwd: sp.Cwd})
}

// expandHome replaces a leading "~" (alone or before a separator) in a
// spawn's cwd with this user's home directory. "~user" is left as it is.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !(runtime.GOOS == "windows" && strings.HasPrefix(p, `~\`)) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cwd %q: %w", p, err)
	}
	return filepath.Join(home, p[1:]), nil
}

func (n *Node) session(id string) (*session, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, ok := n.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no session %s on %s", id, n.cfg.Host)
	}
	return s, nil
}

func (s *session) handle() (adapter.Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.Stopped {
		return nil, errors.New("session is stopped")
	}
	if s.h == nil {
		return nil, errors.New("session is not running on its node (see its status)")
	}
	return s.h, nil
}

// quoteHeaders marks lines in a relayed message that look like Switchworks's
// own header, so one agent cannot pass off text as coming from a person or
// another sender. Carriage returns count as line breaks, and leading spaces,
// invisible characters and case are ignored when matching.
func quoteHeaders(text string) string {
	// Every character a renderer or model might take as a line break.
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n", "\u0085", "\n", "\v", "\n", "\f", "\n").Replace(text)
	lines := strings.Split(text, "\n")
	tag := "[" + strings.ToLower(version.Slug)
	for i, l := range lines {
		bare := strings.TrimLeftFunc(l, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || r == '>' || unicode.IsControl(r)
		})
		if strings.HasPrefix(strings.ToLower(bare), tag) {
			lines[i] = "(quoted) " + l
		}
	}
	return strings.Join(lines, "\n")
}

// headerless reports whether m reaches the agent as it is: a plain message
// sent with the owner's credential.
func headerless(m queuedMsg) bool {
	return strings.HasPrefix(m.From, "user:") && (m.Kind == "" || m.Kind == model.MsgNotify) && m.Owner
}

// formatMessage renders a queued message for the agent. Plain messages
// sent with the owner's credential are passed through; everything else,
// including a person's message sent with any other token (which an agent
// on a host holding it could send too), carries a one-line header saying
// who sent it, with which credential, and how to answer.
func formatMessage(m queuedMsg) string {
	kind := m.Kind
	if kind == "" {
		kind = model.MsgNotify
	}
	if headerless(m) {
		return m.Text
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s msg=%s from=%s", version.Slug, m.ID, m.From)
	if m.Via != "" {
		fmt.Fprintf(&b, " via=%s", m.Via)
	}
	fmt.Fprintf(&b, " kind=%s", kind)
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, " re=%s", m.ReplyTo)
	}
	b.WriteString("]\n")
	switch kind {
	case model.MsgAsk:
		fmt.Fprintf(&b, "(%s is waiting for an answer: reply with the fleet_reply tool, message_id=%s.)\n", m.From, m.ID)
	case model.MsgReply:
		fmt.Fprintf(&b, "(This answers your earlier fleet_ask %s.)\n", m.ReplyTo)
	}
	if n := len(m.Attachments); n > 0 {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		fmt.Fprintf(&b, "(%d image%s attached, in this turn's input: %s.)\n", n, plural, attach.Summary(m.Attachments))
	}
	b.WriteString(quoteHeaders(m.Text))
	return b.String()
}

func containsID(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// deliver accepts a message into a session. Redeliveries of a message it
// has already accepted are acknowledged without effect. Steering falls back
// to queueing when there is no steerable turn, and auto steers only when a
// turn is running; interrupt puts the message at the head of its part of
// the queue and cancels the running turn.
func (n *Node) deliver(ctx context.Context, s *session, m *wire.MessageCmd) (json.RawMessage, error) {
	if m == nil || strings.TrimSpace(m.Text) == "" {
		return nil, errors.New("deliver: empty message")
	}
	h, err := s.handle()
	if err != nil {
		return nil, err
	}
	// Images are checked again here (type by their bytes, limits), and
	// refused before anything is queued if the session cannot take them.
	images, err := attach.Normalize(m.Attachments)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 && !h.Caps().Images {
		return nil, noImages(s.rec.Harness)
	}
	s.mu.Lock()
	dup := containsID(s.rec.Seen, m.ID)
	if !dup {
		for _, q := range s.rec.Queue {
			if q.ID == m.ID {
				dup = true
			}
		}
	}
	if dup {
		s.mu.Unlock()
		return model.Marshal(wire.DeliverResult{Mode: model.ModeQueue, Duplicate: true}), nil
	}
	s.rec.Seen = append(s.rec.Seen, m.ID)
	if len(s.rec.Seen) > seenCap {
		s.rec.Seen = s.rec.Seen[len(s.rec.Seen)-seenCap:]
	}
	s.mu.Unlock()
	msg := queuedMsg{ID: m.ID, From: m.From, Via: m.Via, Owner: m.Owner, Text: m.Text, Kind: m.Kind, ReplyTo: m.ReplyTo,
		Attachments: model.MetaOnly(images)}
	unseen := func() {
		s.mu.Lock()
		for i, id := range s.rec.Seen {
			if id == m.ID {
				s.rec.Seen = append(s.rec.Seen[:i:i], s.rec.Seen[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}
	caps := h.Caps()
	if caps.Steer && (m.Mode == model.ModeSteer || (m.Mode == model.ModeAuto && s.turnRunning(h))) {
		err := n.steer(s, h, msg, images)
		if err == nil {
			_ = n.saveState()
			s.emit(model.KindMessageDelivered, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeSteer, Attachments: msg.Attachments})
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer}), nil
		}
		if errors.Is(err, adapter.ErrNoImages) {
			unseen()
			return nil, err
		}
		if m.Mode == model.ModeSteer || !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: steer failed (%v); queueing instead", s.rec.Path, err)
		}
	}
	// imgMu keeps gcImages from removing these files before the message
	// is in the queue.
	s.imgMu.Lock()
	if _, err := n.storeImages(s.rec.ID, images); err != nil {
		s.imgMu.Unlock()
		unseen()
		return nil, fmt.Errorf("store images: %w", err)
	}
	switch {
	case m.Mode == model.ModeInterrupt && caps.Interrupt:
		s.mu.Lock()
		s.rec.Queue = enqueue(s.rec.Queue, msg, true)
		s.reportQueueLocked()
		s.mu.Unlock()
		s.imgMu.Unlock()
		_ = n.saveState()
		s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeInterrupt, Attachments: msg.Attachments})
		if err := h.Interrupt(ctx); err != nil && !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: interrupt for message %s: %v", s.rec.Path, msg.ID, err)
		}
		go n.tryDeliver(s)
		return model.Marshal(wire.DeliverResult{Mode: model.ModeInterrupt}), nil
	}
	s.mu.Lock()
	s.rec.Queue = enqueue(s.rec.Queue, msg, false)
	s.reportQueueLocked()
	s.mu.Unlock()
	s.imgMu.Unlock()
	_ = n.saveState()
	s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeQueue, Attachments: msg.Attachments})
	go n.tryDeliver(s)
	return model.Marshal(wire.DeliverResult{Mode: model.ModeQueue}), nil
}

// Queue stall recovery. A harness can answer ErrBusy while the session
// reports idle: Codex reports the thread idle a moment before the turn's
// turn/completed, and a turn that never completes leaves it holding a turn
// id. The idle status that would trigger delivery has then come and gone,
// so such a delivery is retried with backoff, and sweepQueues retries any
// session found idle with a queue twice in a row. Variables so tests can
// shorten them.
var (
	busyBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	sweepEvery  = 15 * time.Second // how often the ticker sweeps
	stallAfter  = 30 * time.Second // idle with a queue this long counts as stalled
)

// retryBusy schedules another delivery attempt for a session whose harness
// answered ErrBusy while idle.
func (n *Node) retryBusy(s *session, wait time.Duration) {
	time.AfterFunc(wait, func() {
		select {
		case <-n.closing:
			return
		default:
		}
		n.tryDeliver(s)
	})
}

// sweepQueues retries delivery for sessions that have stayed idle with
// queued messages since the last sweep, and logs a warning for each: with
// a working harness that never lasts more than a moment.
func (n *Node) sweepQueues(now time.Time) {
	for _, s := range n.sessionList() {
		s.mu.Lock()
		stuck := !s.rec.Stopped && s.h != nil && !s.delivering && len(s.rec.Queue) > 0 && s.h.Status() == model.StatusIdle
		kick := false
		switch {
		case !stuck:
			s.stuckSince = time.Time{}
		case s.stuckSince.IsZero():
			s.stuckSince = now
		case now.Sub(s.stuckSince) >= stallAfter:
			kick, s.stuckSince = true, now
		}
		path, depth := s.rec.Path, len(s.rec.Queue)
		s.mu.Unlock()
		if kick {
			n.logf("warning: session %s is idle with %d queued message(s) and no turn starting; retrying delivery", path, depth)
			go n.tryDeliver(s)
		}
	}
}

// turnRunning reports whether the session has a turn in progress.
func (s *session) turnRunning(h adapter.Handle) bool {
	s.mu.Lock()
	active := s.activeTurn != ""
	s.mu.Unlock()
	st := h.Status()
	return active || st == model.StatusActive || st == model.StatusWaitingApproval
}

// steerReq is a message waiting to be steered into a running turn.
type steerReq struct {
	msg    queuedMsg
	images []model.Attachment
	done   chan error
}

// steer delivers msg into the session's running turn and returns the
// harness's answer. Messages that arrive while a steer is in flight wait
// for it, then go in together as one input, each with its own header (as
// in a coalesced turn), rather than one steer each.
func (n *Node) steer(s *session, h adapter.Handle, msg queuedMsg, images []model.Attachment) error {
	req := &steerReq{msg: msg, images: images, done: make(chan error, 1)}
	s.mu.Lock()
	s.steerBuf = append(s.steerBuf, req)
	lead := !s.steering
	s.steering = true
	s.mu.Unlock()
	if lead {
		go n.flushSteers(s, h)
	}
	return <-req.done
}

// flushSteers delivers the waiting steers, a batch at a time, until none
// are left.
func (n *Node) flushSteers(s *session, h adapter.Handle) {
	for {
		s.mu.Lock()
		msgs := make([]queuedMsg, len(s.steerBuf))
		for i, r := range s.steerBuf {
			msgs[i] = r.msg
		}
		k := batchLen(msgs)
		if k == 0 {
			s.steering = false
			s.mu.Unlock()
			return
		}
		batch := s.steerBuf[:k:k]
		s.steerBuf = s.steerBuf[k:]
		s.mu.Unlock()
		var images []model.Attachment
		for _, r := range batch {
			images = append(images, r.images...)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := h.Deliver(ctx, batchInput(msgs[:k], images), adapter.DeliverSteer)
		cancel()
		for _, r := range batch {
			r.done <- err
		}
	}
}

// tryDeliver starts a turn with the next queued message if the session is idle.
func (n *Node) tryDeliver(s *session) {
	for {
		s.mu.Lock()
		if s.delivering {
			// Delivering already: if that attempt finds the harness busy,
			// this trigger (often the turn.completed it was waiting for)
			// means it should try again at once.
			s.redeliver = true
			s.mu.Unlock()
			return
		}
		if s.h == nil || s.rec.Stopped || s.h.Status() != model.StatusIdle {
			s.mu.Unlock()
			return
		}
		if len(s.rec.Queue) == 0 {
			// Idle with nothing left to do: report that the session settled.
			if s.busy && s.activeTurn == "" {
				s.busy = false
				text := s.lastText
				if len(text) > 8000 {
					text = text[:8000]
				}
				s.flushDeltaLocked()
				s.appendLocked(model.KindSessionSettled, model.Settled{TurnID: s.lastTurn, TurnStatus: s.lastStatus, LastMessage: text})
				s.mu.Unlock()
				n.wakeSender()
				return
			}
			s.mu.Unlock()
			return
		}
		s.delivering, s.redeliver = true, false
		batch := append([]queuedMsg(nil), s.rec.Queue[:batchLen(s.rec.Queue)]...)
		h := s.h
		// Record delivery before the turn's own events, so the log reads in
		// order; a failure is recorded right after.
		s.flushDeltaLocked()
		for _, m := range batch {
			s.appendLocked(model.KindMessageDelivered, model.MessageEvent{MessageID: m.ID, From: m.From, Mode: "queue", Attachments: m.Attachments})
		}
		id := s.rec.ID
		s.mu.Unlock()
		n.wakeSender()

		var images []model.Attachment
		var err error
		for _, m := range batch {
			var im []model.Attachment
			if im, err = n.loadImages(id, m.Attachments); err != nil {
				break
			}
			images = append(images, im...)
		}
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			err = h.Deliver(ctx, batchInput(batch, images), adapter.DeliverQueue)
			cancel()
		}

		s.mu.Lock()
		s.delivering = false
		if errors.Is(err, adapter.ErrBusy) {
			// Not delivered after all: they stay at the head of the queue and
			// are retried on the next idle status, at once if one came in
			// meanwhile, or after a backoff if the session says it is idle
			// already (no such status may come).
			again := s.redeliver
			s.redeliver = false
			var wait time.Duration
			if !again && s.rec.Status == model.StatusIdle {
				wait = busyBackoff[min(s.busyTries, len(busyBackoff)-1)]
				s.busyTries++
			}
			for _, m := range batch {
				s.appendLocked(model.KindMessageQueued, model.MessageEvent{MessageID: m.ID, From: m.From, Mode: "queue", Error: "session busy; requeued"})
			}
			path, tries := s.rec.Path, s.busyTries
			s.mu.Unlock()
			n.wakeSender()
			switch {
			case again:
				continue
			case wait > 0:
				n.logf("session %s: harness busy although idle (attempt %d); retrying delivery in %s", path, tries, wait)
				n.retryBusy(s, wait)
			}
			return
		}
		s.busyTries = 0
		s.rec.Queue = without(s.rec.Queue, batch)
		s.reportQueueLocked()
		s.mu.Unlock()
		hasImages := false
		for _, m := range batch {
			if err != nil {
				s.emit(model.KindMessageFailed, model.MessageEvent{MessageID: m.ID, From: m.From, Error: err.Error()})
			}
			hasImages = hasImages || len(m.Attachments) > 0
		}
		n.wakeSender()
		_ = n.saveState()
		if hasImages {
			n.gcImages(s)
		}
		// Loop: if the turn already finished (or failed to start), the next
		// queued messages go now; otherwise the idle status triggers it.
	}
}

// Coalescing: an idle session's queued messages go in together, as one
// turn, rather than one turn each (every turn resends the session's whole
// context to the model). A batch stays within these bounds, and within one
// message's image limits; the first message always goes.
const (
	maxBatchMessages = 50
	maxBatchText     = 256 << 10
)

// batchLen is how many of the queue's first messages go in one input.
func batchLen(q []queuedMsg) int {
	text, imgs := 0, 0
	var imgBytes int64
	for i, m := range q {
		text += len(m.Text)
		imgs += len(m.Attachments)
		for _, a := range m.Attachments {
			imgBytes += a.Size
		}
		if i > 0 && (i >= maxBatchMessages || text > maxBatchText || imgs > attach.MaxImages || imgBytes > attach.MaxTotalBytes) {
			return i
		}
	}
	return len(q)
}

// batchInput joins messages into one input, each with its own header, and
// their images in the same order. The owner's plain messages, which have no
// header, go first, so no text without a header follows another sender's.
func batchInput(batch []queuedMsg, images []model.Attachment) adapter.Input {
	if len(batch) == 1 {
		return adapter.Input{Text: formatMessage(batch[0]), ClientMessageID: batch[0].ID, Images: images}
	}
	byID := map[string][]model.Attachment{}
	off := 0
	for _, m := range batch {
		k := min(off+len(m.Attachments), len(images))
		byID[m.ID] = images[off:k]
		off = k
	}
	ordered := append([]queuedMsg(nil), batch...)
	sort.SliceStable(ordered, func(i, j int) bool { return headerless(ordered[i]) && !headerless(ordered[j]) })
	parts := make([]string, len(ordered))
	var imgs []model.Attachment
	for i, m := range ordered {
		parts[i] = formatMessage(m)
		imgs = append(imgs, byID[m.ID]...)
	}
	return adapter.Input{Text: strings.Join(parts, "\n\n"), ClientMessageID: ordered[0].ID, Images: imgs}
}

// without returns q without the messages of batch.
func without(q, batch []queuedMsg) []queuedMsg {
	drop := map[string]bool{}
	for _, m := range batch {
		drop[m.ID] = true
	}
	out := q[:0:0]
	for _, m := range q {
		if !drop[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// Queue order: the owner's plain messages first, then other people's, then
// agents' and Switchworks's own, each in the order they came. A backlog of
// agent messages then never holds back a person's.
func queueRank(m queuedMsg) int {
	switch {
	case headerless(m):
		return 0
	case strings.HasPrefix(m.From, "user:"):
		return 1
	}
	return 2
}

// enqueue adds m behind the messages that rank with or above it; with
// head (an interrupt), ahead of those that rank with it.
func enqueue(q []queuedMsg, m queuedMsg, head bool) []queuedMsg {
	r := queueRank(m)
	i := 0
	for i < len(q) && (queueRank(q[i]) < r || (!head && queueRank(q[i]) == r)) {
		i++
	}
	return append(q[:i:i], append([]queuedMsg{m}, q[i:]...)...)
}

// reportQueueLocked emits session.queue when the queue's length differs
// from the one last reported, so the coordinator can show it.
func (s *session) reportQueueLocked() {
	if d := len(s.rec.Queue); d != s.reportedQueue {
		s.reportedQueue = d
		s.flushDeltaLocked()
		s.appendLocked(model.KindSessionQueue, model.SessionQueue{Queued: d})
	}
}

func (n *Node) stop(ctx context.Context, s *session) error {
	s.mu.Lock()
	if s.rec.Stopped {
		s.mu.Unlock()
		return nil
	}
	h := s.h
	s.mu.Unlock()
	if h != nil {
		if err := h.Stop(ctx); err != nil {
			return err
		}
	} else {
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusStopped})
	}
	s.mu.Lock()
	s.rec.Stopped = true
	s.rec.Status = model.StatusStopped
	s.h = nil
	id := s.rec.ID
	s.mu.Unlock()
	n.dropImages(id)
	return n.saveState()
}

// setPolicy changes a running session's approval policy and keeps it for
// a resume.
func (n *Node) setPolicy(ctx context.Context, s *session, policy string) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	h, err := s.handle()
	if err != nil {
		return err
	}
	ps, ok := h.(adapter.PolicySetter)
	if !ok || !h.Caps().Policy {
		s.mu.Lock()
		harness := s.rec.Harness
		s.mu.Unlock()
		return fmt.Errorf("%s sessions cannot change their approval policy while running", harness)
	}
	if err := ps.SetApprovalPolicy(ctx, policy); err != nil {
		return err
	}
	s.mu.Lock()
	s.rec.Spec.ApprovalPolicy = policy
	s.mu.Unlock()
	s.emit(model.KindSessionPolicy, model.SessionPolicy{ApprovalPolicy: policy})
	return n.saveState()
}

// handleCmd executes a coordinator command.
func (n *Node) handleCmd(cmd wire.Cmd) wire.Result {
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	var data json.RawMessage
	err := func() error {
		if cmd.Op == wire.OpSpawn {
			var err error
			data, err = n.spawn(ctx, cmd.Spawn)
			return err
		}
		s, err := n.session(cmd.Session)
		if err != nil {
			return err
		}
		switch cmd.Op {
		case wire.OpDeliver:
			var err error
			data, err = n.deliver(ctx, s, cmd.Message)
			return err
		case wire.OpInterrupt:
			h, err := s.handle()
			if err != nil {
				return err
			}
			return h.Interrupt(ctx)
		case wire.OpResolve:
			if cmd.Resolve == nil {
				return errors.New("resolve: missing body")
			}
			h, err := s.handle()
			if err != nil {
				return err
			}
			return h.Resolve(ctx, cmd.Resolve.ApprovalID, cmd.Resolve.Decision)
		case wire.OpStop:
			return n.stop(ctx, s)
		case wire.OpPolicy:
			if cmd.Policy == nil {
				return errors.New("policy: missing body")
			}
			return n.setPolicy(ctx, s, cmd.Policy.ApprovalPolicy)
		}
		return fmt.Errorf("unknown op %q", cmd.Op)
	}()
	if err != nil {
		return wire.Result{OK: false, Error: err.Error()}
	}
	return wire.Result{OK: true, Data: data}
}

// Close stops adapters (harness processes keep their own history) and
// flushes state.
func (n *Node) Close() error {
	close(n.closing)
	// Report "stopped" now, not after the teardown below: the process is
	// going away either way, and `swk service stop` waits on the process.
	n.status.Stop()
	if n.local != nil {
		n.local.close()
	}
	n.connMu.Lock()
	if n.conn != nil {
		_ = n.conn.Close()
	}
	n.connMu.Unlock()
	n.mu.Lock()
	ads := make([]adapter.Adapter, 0, len(n.adapters))
	for _, a := range n.adapters {
		ads = append(ads, a)
	}
	n.mu.Unlock()
	for _, a := range ads {
		_ = a.Close()
	}
	n.wg.Wait()
	for _, s := range n.sessionList() {
		s.mu.Lock()
		s.flushDeltaLocked()
		if s.spool != nil {
			_ = s.spool.Sync()
			_ = s.spool.Close()
		}
		s.mu.Unlock()
	}
	// The harness log HarnessOptions opened (Windows cannot delete a file
	// that is still open).
	if l, ok := n.hopts.Stderr.(*runstatus.Log); ok {
		_ = l.Close()
	}
	return n.saveState()
}

// sandboxed picks the harnesses that apply a session's sandbox setting.
func sandboxed(names []string) []string {
	out := []string{}
	for _, h := range names {
		if harness.EnforcesSandbox(h) {
			out = append(out, h)
		}
	}
	return out
}

// takesImages picks the harnesses that can be given images.
func takesImages(names []string) []string {
	out := []string{}
	for _, h := range names {
		if harness.TakesImages(h) {
			out = append(out, h)
		}
	}
	return out
}

// hello builds the node's greeting.
func (n *Node) hello() wire.Hello {
	h := wire.Hello{Protocol: wire.ProtocolVersion, Host: n.cfg.Host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Features: []string{wire.FeatureCwdRoot, wire.FeatureAgentSpawn}, Sandboxed: sandboxed(n.Adapters()), Images: takesImages(n.Adapters()),
		Version: version.Version, Adapters: n.Adapters()}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		if !s.rec.Stopped || len(s.unacked) > 0 {
			h.Sessions = append(h.Sessions, wire.SessionState{ID: s.rec.ID, Path: s.rec.Path, Harness: s.rec.Harness,
				Status: s.rec.Status, LastSeq: s.seq, NativeRef: s.rec.NativeRef,
				ApprovalPolicy: s.rec.Spec.ApprovalPolicy, Sandbox: s.rec.Spec.Sandbox})
		}
		s.mu.Unlock()
	}
	return h
}
