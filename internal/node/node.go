// SPDX-License-Identifier: GPL-3.0-or-later

// Package node is `swyd node`: it runs harness sessions on one workstation,
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

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/harness"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/version"
	"github.com/jm2/switchyard/internal/wire"
)

// Config is node.json.
type Config struct {
	Host            string `json:"host"`
	CoordinatorAPI  string `json:"coordinator_api"`
	CoordinatorNode string `json:"coordinator_node"`

	Codex struct {
		Bin  string   `json:"bin,omitempty"`
		Home string   `json:"home,omitempty"` // CODEX_HOME override
		Args []string `json:"args,omitempty"`
	} `json:"codex"`
	Agy struct {
		Bin  string   `json:"bin,omitempty"`
		Args []string `json:"args,omitempty"`
	} `json:"agy"`

	// Defaults applied to spawns that do not set them.
	DefaultApprovalPolicy string `json:"default_approval_policy,omitempty"` // codex: never | on-request | untrusted
	DefaultSandbox        string `json:"default_sandbox,omitempty"`         // read-only | workspace-write | danger-full-access

	// MCP controls the fleet tools attached to each Codex session.
	MCP struct {
		Disabled bool     `json:"disabled,omitempty"`
		Command  string   `json:"command,omitempty"` // default: this swyd binary
		Args     []string `json:"args,omitempty"`    // default: ["mcp"]
	} `json:"mcp"`
}

// spool compaction threshold.
const compactBytes = 1 << 20

type queuedMsg struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Text    string `json:"text"`
	Kind    string `json:"kind,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
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

	mu         sync.Mutex
	rec        record
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

func (s *session) appendLocked(kind string, payload any) {
	s.seq++
	ev := model.Event{ID: ulid.New(), SessionID: s.rec.ID, Seq: s.seq, TS: time.Now().UTC(), Kind: kind,
		Source: s.rec.Harness, Payload: model.Marshal(payload)}
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

// ticker flushes coalesced deltas and fsyncs spools.
func (n *Node) ticker() {
	defer n.wg.Done()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	syncEvery := 0
	for {
		select {
		case <-t.C:
		case <-n.closing:
			return
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
	h, err := a.Resume(ctx, *rec.NativeRef, spec, s.sink)
	if err != nil {
		n.logf("session %s: resume failed: %v", rec.Path, err)
		s.emit(model.KindError, model.ErrorEvent{Message: "resume: " + err.Error()})
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusError, Detail: "resume failed"})
		return
	}
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
	n.logf("session %s resumed (%s %s)", rec.Path, rec.Harness, rec.NativeRef.ID)
	go n.tryDeliver(s)
}

func (n *Node) spawn(ctx context.Context, cmd *wire.SpawnCmd) (json.RawMessage, error) {
	if cmd == nil {
		return nil, errors.New("spawn: missing body")
	}
	n.mu.Lock()
	if existing, ok := n.sessions[cmd.Session.ID]; ok {
		n.mu.Unlock()
		existing.mu.Lock()
		ref := existing.rec.NativeRef
		existing.mu.Unlock()
		return model.Marshal(map[string]any{"native_ref": ref}), nil // idempotent retry
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
	if fi, err := os.Stat(spec.Cwd); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory on %s", spec.Cwd, n.cfg.Host)
	}
	s := &session{n: n, rec: record{
		ID: cmd.Session.ID, Path: cmd.Session.Path, Harness: harnessName, Status: model.StatusStarting, CreatedAt: time.Now(),
		Spec: adapter.StartSpec{Name: cmd.Session.Path, Cwd: spec.Cwd, Model: spec.Model, Provider: spec.Provider,
			ApprovalPolicy: approval, Sandbox: sandbox},
	}}
	if err := s.openSpool(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.sessions[s.rec.ID] = s
	n.mu.Unlock()
	startSpec := s.rec.Spec
	startSpec.MCP = n.mcpFor(s.rec.ID, s.rec.Path)
	h, err := a.Start(ctx, startSpec, s.sink)
	if err != nil {
		s.emit(model.KindError, model.ErrorEvent{Message: "start: " + err.Error()})
		s.emit(model.KindSessionStatus, model.SessionStatus{Status: model.StatusError, Detail: "start failed"})
		s.mu.Lock()
		s.rec.Stopped = true
		s.mu.Unlock()
		_ = n.saveState()
		return nil, err
	}
	s.mu.Lock()
	s.h = h
	if strings.TrimSpace(spec.Prompt) != "" {
		s.rec.Queue = append(s.rec.Queue, queuedMsg{ID: ulid.New(), From: "user:spawn", Text: spec.Prompt})
	}
	ref := s.rec.NativeRef
	s.mu.Unlock()
	_ = n.saveState()
	n.logf("session %s started (%s)", s.rec.Path, harnessName)
	go n.tryDeliver(s)
	return model.Marshal(map[string]any{"native_ref": ref}), nil
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

// formatMessage renders a queued message for the agent. Plain messages
// from people are passed through; everything else carries a one-line header
// saying who sent it and how to answer.
func formatMessage(m queuedMsg) string {
	kind := m.Kind
	if kind == "" {
		kind = model.MsgNotify
	}
	if strings.HasPrefix(m.From, "user:") && kind == model.MsgNotify {
		return m.Text
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s msg=%s from=%s kind=%s", version.Slug, m.ID, m.From, kind)
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
	b.WriteString(m.Text)
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
// to queueing when there is no steerable turn; interrupt puts the message at
// the head of the queue and cancels the running turn.
func (n *Node) deliver(ctx context.Context, s *session, m *wire.MessageCmd) (json.RawMessage, error) {
	if m == nil || strings.TrimSpace(m.Text) == "" {
		return nil, errors.New("deliver: empty message")
	}
	h, err := s.handle()
	if err != nil {
		return nil, err
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
	msg := queuedMsg{ID: m.ID, From: m.From, Text: m.Text, Kind: m.Kind, ReplyTo: m.ReplyTo}
	caps := h.Caps()
	switch {
	case m.Mode == model.ModeSteer && caps.Steer:
		err := h.Deliver(ctx, adapter.Input{Text: formatMessage(msg), ClientMessageID: msg.ID}, adapter.DeliverSteer)
		if err == nil {
			_ = n.saveState()
			s.emit(model.KindMessageDelivered, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeSteer})
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer}), nil
		}
		n.logf("session %s: steer failed (%v); queueing instead", s.rec.Path, err)
	case m.Mode == model.ModeInterrupt && caps.Interrupt:
		s.mu.Lock()
		s.rec.Queue = append([]queuedMsg{msg}, s.rec.Queue...)
		s.mu.Unlock()
		_ = n.saveState()
		s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeInterrupt})
		if err := h.Interrupt(ctx); err != nil && !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: interrupt for message %s: %v", s.rec.Path, msg.ID, err)
		}
		go n.tryDeliver(s)
		return model.Marshal(wire.DeliverResult{Mode: model.ModeInterrupt}), nil
	}
	s.mu.Lock()
	s.rec.Queue = append(s.rec.Queue, msg)
	s.mu.Unlock()
	_ = n.saveState()
	s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeQueue})
	go n.tryDeliver(s)
	return model.Marshal(wire.DeliverResult{Mode: model.ModeQueue}), nil
}

// tryDeliver starts a turn with the next queued message if the session is idle.
func (n *Node) tryDeliver(s *session) {
	for {
		s.mu.Lock()
		if s.delivering || s.h == nil || s.rec.Stopped || s.h.Status() != model.StatusIdle {
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
		s.delivering = true
		msg := s.rec.Queue[0]
		h := s.h
		// Record delivery before the turn's own events, so the log reads in
		// order; a failure is recorded right after.
		s.flushDeltaLocked()
		s.appendLocked(model.KindMessageDelivered, model.MessageEvent{MessageID: msg.ID, From: msg.From, Mode: "queue"})
		s.mu.Unlock()
		n.wakeSender()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := h.Deliver(ctx, adapter.Input{Text: formatMessage(msg), ClientMessageID: msg.ID}, adapter.DeliverQueue)
		cancel()

		s.mu.Lock()
		s.delivering = false
		if errors.Is(err, adapter.ErrBusy) {
			// Not delivered after all: it stays at the head of the queue and
			// is retried on the next idle status.
			s.appendLocked(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Mode: "queue", Error: "session busy; requeued"})
			s.mu.Unlock()
			n.wakeSender()
			return
		}
		if len(s.rec.Queue) > 0 && s.rec.Queue[0].ID == msg.ID {
			s.rec.Queue = s.rec.Queue[1:]
		}
		s.mu.Unlock()
		if err != nil {
			s.emit(model.KindMessageFailed, model.MessageEvent{MessageID: msg.ID, From: msg.From, Error: err.Error()})
		}
		_ = n.saveState()
		// Loop: if the turn already finished (or failed to start), the next
		// queued message goes now; otherwise the idle status triggers it.
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
	s.mu.Unlock()
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
	return n.saveState()
}

// hello builds the node's greeting.
func (n *Node) hello() wire.Hello {
	h := wire.Hello{Protocol: wire.ProtocolVersion, Host: n.cfg.Host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version.Version, Adapters: n.Adapters()}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		if !s.rec.Stopped || len(s.unacked) > 0 {
			h.Sessions = append(h.Sessions, wire.SessionState{ID: s.rec.ID, Path: s.rec.Path, Harness: s.rec.Harness,
				Status: s.rec.Status, LastSeq: s.seq, NativeRef: s.rec.NativeRef})
		}
		s.mu.Unlock()
	}
	return h
}
