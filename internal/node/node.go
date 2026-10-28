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

	// ContinueInterrupted (default true): when the node starts again after
	// going down during a session's turn (an upgrade, a reboot, a crash),
	// or after its harness exited mid-turn and was restarted (Codex's
	// app-server crashing), it gives the session a system message that
	// starts a new turn, saying its previous turn was cut off, so the
	// agent carries on instead of sitting idle until someone prompts it.
	// false turns that off.
	ContinueInterrupted *bool `json:"continue_interrupted,omitempty"`

	// FilesDir is where files received with messages are written for the
	// sessions to read (default: <user cache dir>/switchworks/files; see
	// files.go). Keep it out of the directories a sandbox profile denies.
	FilesDir string `json:"files_dir,omitempty"`
	// FilesQuotaMiB caps the received files kept on this node, all
	// sessions together (default defaultFilesQuotaMiB; -1 for no cap).
	// A message whose files would pass it is refused.
	FilesQuotaMiB int64 `json:"files_quota_mib,omitempty"`

	// AttachDeny lists more files and directories agents may never attach
	// (images, files), besides Switchworks's own and the credential
	// locations the node always denies (see attachPolicy). "~/" is the
	// home directory.
	AttachDeny []string `json:"attach_deny,omitempty"`
	// AttachAllow lists files and directories exempt from the rules on
	// names that look like credentials (.env, id_ed25519) and on dot-files
	// when a session works in the home directory. It never lifts a deny.
	AttachAllow []string `json:"attach_allow,omitempty"`
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
	// ReplyToText: the start of the ask a reply answers.
	ReplyToText string `json:"reply_to_text,omitempty"`
	// AtStep: a steer into the running turn, held until that turn's next
	// step (when the agent would read it anyway), so it can still be
	// cancelled. If the turn ends first it starts the next one.
	AtStep bool `json:"at_step,omitempty"`
	// Attachments: metadata only. The bytes wait in the session's
	// attachment directory (see images.go) until the message is delivered.
	Attachments []model.Attachment `json:"attachments,omitempty"`
	// Files were written to disk when the message was accepted (files.go).
	Files []fileRef `json:"files,omitempty"`
	// ExpiresAt, when set, is when the message expires: past it, it is
	// dropped undelivered rather than starting or joining a turn.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Quiet: it never starts a turn of its own. Steered into a running
	// turn as usual, it otherwise waits here until a turn starts for
	// another message (and goes in with it) or a person promotes it.
	Quiet bool `json:"quiet,omitempty"`
	// Promoted: a person asked for it now (OpPromote), so it may start a
	// turn even if it is quiet.
	Promoted bool `json:"promoted,omitempty"`
	// Sending is set, and saved, before the message is handed to the
	// harness (a new turn, or a promoted message's steer). It stays in the
	// queue meanwhile, out of reach of promote, cancel and expiry; one
	// still marked after a restart may or may not have gone in, and is
	// reported failed rather than delivered a second time.
	Sending bool `json:"sending,omitempty"`
}

// wakes reports whether m may start a turn: it is not quiet, or a person
// promoted it.
func (m queuedMsg) wakes() bool { return !m.Quiet || m.Promoted }

// expired reports whether m has an expiry and it has passed.
func (m queuedMsg) expired(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && now.After(m.ExpiresAt)
}

// errExpired is the answer to a steer whose message expired while it waited.
var errExpired = errors.New("the message expired before it could be delivered")

// Deduplication of coordinator redeliveries: each accepted message id is
// kept until the coordinator can no longer redeliver it. It redelivers a
// message while it is pending, until its expires_at, so an id is kept an
// hour past that (clock skew); a message without one (from a coordinator
// before 0.1.5) is kept for seenFor, past the coordinator's default TTL of
// 24 h. seenCap bounds the list; past it the ids due to go first go.
const (
	seenCap   = 4096
	seenFor   = 25 * time.Hour
	seenSlack = time.Hour
)

// acceptedID is a message id kept for deduplication, and until when.
type acceptedID struct {
	ID    string    `json:"id"`
	Until time.Time `json:"until"`
}

// Queue limits, per session: messages and text waiting. The owner's plain
// messages may use all of it; everyone else's (other people, agents,
// Switchworks's notices) only otherQueueShare of it, so a flood from
// agents never leaves the owner unable to reach their session. Queued
// images are bounded per node (their bytes wait on disk until delivered),
// with the same share.
const (
	maxQueueMessages    = 256
	maxQueueText        = 4 << 20
	maxQueuedImageBytes = 256 << 20
)

// share is the part of limit m's sender may use: all of it for the owner's
// plain messages, three quarters for the rest.
func share(m queuedMsg, limit int64) int64 {
	if headerless(m) {
		return limit
	}
	return limit * 3 / 4
}

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
	// Accepted lists recently accepted message ids; Seen is the list of
	// ids nodes before 0.1.5 kept instead, read once and converted.
	Accepted  []acceptedID `json:"accepted,omitempty"`
	Seen      []string     `json:"seen,omitempty"`
	Stopped   bool         `json:"stopped,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	// ActiveTurn is the turn running when the record was saved: saved as
	// a turn starts and cleared as it completes, or as a person interrupts
	// it. Set when the node starts, the turn was cut off by the node going
	// down (see Config.ContinueInterrupted).
	ActiveTurn string `json:"active_turn,omitempty"`
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
	stepping bool // stepSteers is running
	steerBuf []*steerReq
	// solo holds queued messages that failed in a batch: each is retried
	// on its own, so only one that fails alone is reported failed.
	solo map[string]bool
	// announce holds the batch being handed to the harness as a new turn.
	// Its message.delivered events are recorded once it is known to have
	// gone in: when the turn starts (so they come before the turn's own
	// events) or when Deliver returns.
	announce []queuedMsg

	// lostTurn: the running turn failed because the harness exited under
	// it; once the harness's restart resumes the session, it is continued.
	lostTurn bool

	// Settled detection: busy is set when a turn starts and cleared once the
	// session is idle with an empty queue, at which point session.settled is
	// emitted with the last turn's outcome.
	activeTurn string
	busy       bool
	lastTurn   string
	lastStatus string
	lastText   string

	// Receipts: the messages each turn carried (turnMsgs), the messages
	// given to the harness as a new turn whose turn.started has not come
	// yet (nextTurn), and each such turn's last agent message (turnReply).
	// When the turn completes, each of its messages is reported seen.
	turnMsgs  map[string][]string
	nextTurn  []string
	turnReply map[string]string
	// doneTurns are the last turns that completed, for a steer that went
	// into one which completed before the steer's answer was read.
	doneTurns []doneTurn
}

// doneTurn is a completed turn: its status and last agent message.
type doneTurn struct{ id, status, reply string }

// maxDoneTurns bounds session.doneTurns.
const maxDoneTurns = 8

// carriedLocked records that turn carries the messages ids, for their
// message.seen when it completes.
func (s *session) carriedLocked(turn string, ids []string) {
	if turn == "" || len(ids) == 0 {
		return
	}
	if s.turnMsgs == nil {
		s.turnMsgs = map[string][]string{}
	}
	s.turnMsgs[turn] = append(s.turnMsgs[turn], ids...)
}

// seenLocked reports the messages a completed turn carried as seen, with
// the turn's last agent message. A turn that failed or was interrupted
// may have ended before the model read them, so its messages stay
// delivered.
func (s *session) seenLocked(p model.TurnCompleted) {
	ids, reply := s.turnMsgs[p.TurnID], s.turnReply[p.TurnID]
	s.doneTurns = append(s.doneTurns, doneTurn{p.TurnID, p.Status, reply})
	if len(s.doneTurns) > maxDoneTurns {
		s.doneTurns = s.doneTurns[len(s.doneTurns)-maxDoneTurns:]
	}
	delete(s.turnMsgs, p.TurnID)
	delete(s.turnReply, p.TurnID)
	for t := range s.turnMsgs {
		if t != s.activeTurn {
			// A turn that ended without a turn.completed (as when the
			// harness restarted): its messages are not known to be seen.
			delete(s.turnMsgs, t)
			delete(s.turnReply, t)
		}
	}
	if p.Status != "completed" {
		return
	}
	for _, id := range ids {
		s.appendLocked(model.KindMessageSeen, model.MessageSeen{MessageID: id, TurnID: p.TurnID, ItemID: reply})
	}
}

// steeredLocked records that a steer carried the messages ids into the
// turn that was running when it was sent (into; empty if the node had not
// seen it start). That turn may have completed while the steer's answer
// was on its way: then they are seen now if it completed normally, and
// credited to no other turn.
func (s *session) steeredLocked(into string, ids []string) {
	if into == "" {
		if s.activeTurn != "" {
			s.carriedLocked(s.activeTurn, ids)
		} else {
			s.nextTurn = append(s.nextTurn, ids...) // a turn not yet reported started
		}
		return
	}
	if s.activeTurn == into {
		s.carriedLocked(into, ids)
		return
	}
	for _, d := range s.doneTurns {
		if d.id == into && d.status == "completed" {
			for _, id := range ids {
				s.appendLocked(model.KindMessageSeen, model.MessageSeen{MessageID: id, TurnID: d.id, ItemID: d.reply})
			}
		}
	}
}

// announceLocked records message.delivered for the batch being handed to
// the harness, once.
func (s *session) announceLocked() {
	if len(s.announce) == 0 {
		return
	}
	s.flushDeltaLocked()
	for _, m := range s.announce {
		s.appendLocked(model.KindMessageDelivered, model.MessageEvent{MessageID: m.ID, From: m.From, Mode: model.ModeQueue, Attachments: m.Attachments})
	}
	s.announce = nil
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
	// sendMu orders what goes to the coordinator: the sender's batches,
	// and a session's events ahead of that session's fleet requests.
	sendMu sync.Mutex

	reqMu   sync.Mutex
	reqs    map[string]chan reqReply // node -> coordinator requests in flight
	local   *localAPI
	mcpCmd  string
	mcpArgs []string

	// spawning holds the ids of spawns in progress, and forgotten those of
	// stopped sessions whose records are gone: a spawn for either, or for
	// a session the node has, is refused.
	spawning  map[string]bool
	forgotten map[string]bool

	// imgMu orders checking the node's queued image bytes against storing
	// more (taken before session.imgMu).
	imgMu sync.Mutex

	stateMu sync.Mutex
	filesMu sync.Mutex // orders writeFiles, so the quota holds
	closing chan struct{}
	wg      sync.WaitGroup

	status *runstatus.Reporter // status.json for `swk service status` and the tray
}

// New loads node state from dir and resumes live sessions.
func New(dir string, cfg Config, hopts harness.Options, logf func(string, ...any)) (*Node, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := checkAttachPaths(cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		return nil, err
	}
	n := &Node{dir: dir, cfg: cfg, logf: logf, hopts: hopts,
		adapters: map[string]adapter.Adapter{}, sessions: map[string]*session{},
		spawning: map[string]bool{}, forgotten: map[string]bool{},
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
	changed := false
	for _, rec := range recs {
		s := &session{n: n, rec: rec}
		if err := s.openSpool(); err != nil {
			return nil, fmt.Errorf("session %s spool: %w", rec.ID, err)
		}
		changed = s.recover(time.Now()) || changed
		n.sessions[rec.ID] = s
	}
	if changed {
		n.persist()
	}
	n.forgetStopped()
	n.status = runstatus.Start(dir, runstatus.Status{Role: "node", Host: cfg.Host,
		Coordinator: cfg.CoordinatorNode, WebUI: webUIFor(cfg.CoordinatorAPI)})
	n.wg.Add(3)
	go n.ticker()
	go n.sender()
	go n.filesJanitor()
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

// persist saves state.json where a failure cannot be handed back: it is
// logged, and the next save writes everything again.
func (n *Node) persist() {
	if err := n.saveState(); err != nil {
		n.logf("save node state: %v", err)
	}
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
	if ev.Kind == model.KindTurnStarted {
		// A turn that starts while the node hands a batch to the harness
		// is that batch's: it went in.
		s.announceLocked()
	}
	s.appendLocked(ev.Kind, ev.Payload)
	idle := false
	save := false
	step := false           // the running turn finished a step, with steers held for it
	var lost adapter.Handle // resumed after the harness exited mid-turn
	lostBy := ""
	switch ev.Kind {
	case model.KindTurnStarted:
		if p, ok := ev.Payload.(model.TurnStarted); ok {
			s.activeTurn, s.busy, s.lostTurn = p.TurnID, true, false
			if s.rec.ActiveTurn != p.TurnID {
				s.rec.ActiveTurn, save = p.TurnID, true
			}
			// The turn the node's last delivery started.
			s.carriedLocked(p.TurnID, s.nextTurn)
			s.nextTurn = nil
		}
	case model.KindTurnCompleted:
		if p, ok := ev.Payload.(model.TurnCompleted); ok {
			if s.activeTurn == p.TurnID {
				s.activeTurn = ""
			}
			// A turn that ends because the node is shutting down (its
			// harness stopped), or because the harness process exited under
			// it, stays recorded: it was cut off. After the harness's
			// restart resumes the session, it is continued (lostTurn).
			switch {
			case p.HarnessExited && s.rec.ActiveTurn == p.TurnID:
				s.lostTurn = true
			case s.rec.ActiveTurn != "" && !s.n.isClosing():
				s.rec.ActiveTurn, save = "", true
			}
			s.lastTurn, s.lastStatus = p.TurnID, p.Status
			s.seenLocked(p)
			idle = true // re-check delivery and settling now that the turn is done
		}
	case model.KindItemCompleted:
		if p, ok := ev.Payload.(model.Item); ok && p.TurnID == s.activeTurn && p.ItemType != "userMessage" && s.hasAtStepLocked() {
			step = true
		}
		if p, ok := ev.Payload.(model.Item); ok && p.ItemType == "agentMessage" {
			if p.Text != "" {
				s.lastText = p.Text
			}
			// Kept for the running turn too, for a steer whose answer
			// comes after the turn completed (steeredLocked).
			if _, carried := s.turnMsgs[p.TurnID]; (carried || p.TurnID == s.activeTurn) && p.ItemID != "" {
				if s.turnReply == nil {
					s.turnReply = map[string]string{}
				}
				s.turnReply[p.TurnID] = p.ItemID
			}
		}
	case model.KindSessionRegistered:
		if p, ok := ev.Payload.(model.SessionRegistered); ok {
			ref := p.NativeRef
			s.rec.NativeRef = &ref
			save = true
			// The harness restarted after exiting mid-turn and resumed the
			// session (a node restart's resume sets s.h only afterwards,
			// and continues the turn itself).
			if p.Resumed && s.lostTurn && s.h != nil {
				s.lostTurn = false
				lost, lostBy = s.h, cutByHarness(s.rec.Harness)
			}
		}
	case model.KindSessionStatus:
		if p, ok := ev.Payload.(model.SessionStatus); ok {
			s.rec.Status = p.Status
			idle = p.Status == model.StatusIdle
		}
	}
	s.mu.Unlock()
	if save {
		s.n.persist()
	}
	if idle {
		go s.n.tryDeliver(s)
	}
	if step {
		go s.n.stepSteers(s)
	}
	if lost != nil {
		go s.n.continueInterrupted(s, lost, time.Now(), lostBy)
	}
	s.n.wakeSender()
}

// cutByHarness is what cut a turn off when a harness of this kind exited
// under it, for the message that continues it.
func cutByHarness(harness string) string {
	if harness == "codex" {
		return "Codex restarted"
	}
	return harness + " restarted"
}

// isClosing reports whether the node is shutting down.
func (n *Node) isClosing() bool {
	select {
	case <-n.closing:
		return true
	default:
		return false
	}
}

func (s *session) hasAtStepLocked() bool {
	for _, m := range s.rec.Queue {
		if m.AtStep && !m.Sending {
			return true
		}
	}
	return false
}

// stepSteers steers the messages held for the running turn's next step
// (AtStep) into it, as promote does: the agent reads steered input at its
// next step, so holding them until then costs no time and leaves them
// cancellable meanwhile.
func (n *Node) stepSteers(s *session) {
	s.mu.Lock()
	if s.stepping {
		s.mu.Unlock()
		return
	}
	s.stepping = true
	var ids []string
	for _, m := range s.rec.Queue {
		if m.AtStep && !m.Sending {
			ids = append(ids, m.ID)
		}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stepping = false
		s.mu.Unlock()
	}()
	for _, id := range ids {
		if _, err := n.promote(context.Background(), s, &wire.QueuedCmd{MessageID: id, Mode: model.ModeSteer}); err != nil {
			n.logf("session %s: steer of held message %s: %v", s.rec.Path, id, err)
		}
	}
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
	n.continueInterrupted(s, h, time.Now(), "Switchworks restarted")
	go n.tryDeliver(s)
}

// continueInterrupted queues a system message for a resumed session whose
// turn was cut off (record.ActiveTurn) by the node going down, or by its
// harness exiting, as cutBy says ("Switchworks restarted"), so a new turn
// carries on with it, unless Config.ContinueInterrupted is false or the
// harness kept the turn running. The record's turn is cleared in the same
// save, so each interruption gets one message.
func (n *Node) continueInterrupted(s *session, h adapter.Handle, now time.Time, cutBy string) {
	s.mu.Lock()
	turn := s.rec.ActiveTurn
	s.mu.Unlock()
	if turn == "" {
		return
	}
	on := n.cfg.ContinueInterrupted == nil || *n.cfg.ContinueInterrupted
	if on && s.turnRunning(h) {
		on = false // the harness still runs it: nothing was cut off
	}
	s.mu.Lock()
	if s.rec.Stopped || s.rec.ActiveTurn != turn {
		s.mu.Unlock()
		return
	}
	s.rec.ActiveTurn = ""
	var msg queuedMsg
	if on {
		msg = queuedMsg{ID: ulid.New(), From: model.SystemSender, Kind: model.MsgSystem,
			Text: fmt.Sprintf("Your previous turn (id %s) was cut off when %s at %s. Continue where you left off; check the state of anything you were running.",
				turn, cutBy, now.UTC().Format("2006-01-02 15:04 UTC"))}
		s.rec.Queue = enqueue(s.rec.Queue, msg, false)
		s.reportQueueLocked()
	}
	path := s.rec.Path
	s.mu.Unlock()
	n.persist()
	if on {
		n.logf("session %s: its turn %s was cut off (%s); queued a message to continue it", path, turn, cutBy)
		s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: model.ModeQueue})
		go n.tryDeliver(s)
	}
}

// interrupt cancels the session's running turn on purpose (a person's
// interrupt, or a message sent with it): the record no longer holds it as
// active, so a restart before the turn ends does not continue it.
func (n *Node) interrupt(ctx context.Context, s *session, h adapter.Handle) error {
	s.mu.Lock()
	had := s.rec.ActiveTurn != ""
	s.rec.ActiveTurn = ""
	s.mu.Unlock()
	if had {
		n.persist()
	}
	return h.Interrupt(ctx)
}

func (n *Node) spawn(ctx context.Context, cmd *wire.SpawnCmd) (json.RawMessage, error) {
	if cmd == nil {
		return nil, errors.New("spawn: missing body")
	}
	if !ulid.Valid(cmd.Session.ID) {
		return nil, errors.New("spawn: bad session id")
	}
	// One spawn per id: a second one, even while the first is starting,
	// would start a second harness under the same session.
	n.mu.Lock()
	if _, ok := n.sessions[cmd.Session.ID]; ok || n.spawning[cmd.Session.ID] || n.forgotten[cmd.Session.ID] {
		n.mu.Unlock()
		return nil, fmt.Errorf("spawn: session %s exists on %s already", cmd.Session.ID, n.cfg.Host)
	}
	if n.spawning == nil {
		n.spawning = map[string]bool{}
	}
	n.spawning[cmd.Session.ID] = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.spawning, cmd.Session.ID)
		n.mu.Unlock()
	}()
	spec := cmd.Spec
	harnessName := spec.Harness
	if harnessName == "" {
		harnessName = "codex"
	}
	approval, sandbox := spec.ApprovalPolicy, spec.Sandbox
	if approval == "" {
		approval = n.cfg.DefaultApprovalPolicy
	}
	if sandbox == "" {
		sandbox = n.cfg.DefaultSandbox
	}
	// The coordinator checks these too; a node does not rely on it.
	if err := checkSpawn(harnessName, approval, sandbox, cmd.Session.Path); err != nil {
		return nil, err
	}
	a, err := n.adapterFor(harnessName)
	if err != nil {
		return nil, err
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
	var prompt *queuedMsg
	if strings.TrimSpace(spec.Prompt) != "" {
		prompt = &queuedMsg{ID: ulid.New(), From: "user:spawn", Via: cmd.PromptVia, Owner: cmd.PromptOwner,
			Text: spec.Prompt, Attachments: model.MetaOnly(images)}
	}
	if len(images) > 0 {
		// Only now is it known whether an ACP agent takes images.
		if !h.Caps().Images {
			_ = h.Stop(ctx)
			return failStart(noImages(harnessName))
		}
		n.imgMu.Lock()
		err := n.imageRoom(*prompt)
		if err == nil {
			_, err = n.storeImages(s.rec.ID, images)
		}
		n.imgMu.Unlock()
		if err != nil {
			_ = h.Stop(ctx)
			return failStart(err)
		}
	}
	s.mu.Lock()
	s.h = h
	if prompt != nil {
		s.rec.Queue = append(s.rec.Queue, *prompt)
	}
	ref, sp := s.rec.NativeRef, s.rec.Spec
	s.mu.Unlock()
	if err := n.saveState(); err != nil {
		// Unsaved, the session would be lost at the node's next restart
		// while its harness ran on.
		_ = h.Stop(ctx)
		return failStart(fmt.Errorf("save node state: %w", err))
	}
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

// checkSpawn repeats the coordinator's checks of a spawn's enums and path
// (see coord.validateSpawn), with the node's defaults applied.
func checkSpawn(harnessName, approval, sandbox, path string) error {
	if err := model.ValidateName("harness", harnessName); err != nil {
		return err
	}
	switch approval {
	case "", "never", "on-request", "on-failure", "untrusted":
	default:
		return fmt.Errorf("unknown approval policy %q (never, on-request, on-failure, untrusted)", approval)
	}
	switch sandbox {
	case "", "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("unknown sandbox %q (read-only, workspace-write, danger-full-access)", sandbox)
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || len(parts) > 5 {
		return fmt.Errorf("invalid session path %q", path)
	}
	for _, p := range parts {
		if err := model.ValidateName("session", p); err != nil {
			return err
		}
	}
	return nil
}

// checkMessage repeats the coordinator's checks of a message's id, enums
// and the fields that go into its header, so none can carry a line break
// or a "]" that ends the header early.
func checkMessage(m *wire.MessageCmd) error {
	if !ulid.Valid(m.ID) {
		return errors.New("deliver: bad message id")
	}
	switch m.Mode {
	case "", model.ModeAuto, model.ModeQueue, model.ModeSteer, model.ModeInterrupt:
	default:
		return fmt.Errorf("deliver: unknown mode %q", m.Mode)
	}
	switch m.Kind {
	case "", model.MsgNotify, model.MsgAsk, model.MsgReply, model.MsgDone, model.MsgSystem:
	default:
		return fmt.Errorf("deliver: unknown kind %q", m.Kind)
	}
	if m.From == "" {
		return errors.New("deliver: no sender")
	}
	for what, v := range map[string]string{"sender": m.From, "via": m.Via, "reply_to": m.ReplyTo} {
		if !headerSafe(v) {
			return fmt.Errorf("deliver: invalid %s %q", what, v)
		}
	}
	return nil
}

// headerSafe reports whether v can go in a message header as it is.
func headerSafe(v string) bool {
	if len(v) > 512 || !utf8.ValidString(v) {
		return false
	}
	return strings.IndexFunc(v, func(r rune) bool {
		return r == '[' || r == ']' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) < 0
}

func (n *Node) session(id string) (*session, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, ok := n.sessions[id]
	if !ok {
		if n.forgotten[id] {
			return nil, fmt.Errorf("session %s on %s is stopped: %w", id, n.cfg.Host, errForgotten)
		}
		return nil, fmt.Errorf("no session %s on %s", id, n.cfg.Host)
	}
	return s, nil
}

// errForgotten: the session stopped, and the node has forgotten it.
var errForgotten = errors.New("its record is gone")

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
// another sender. Carriage returns count as line breaks, and a line is
// matched by its skeleton (see looksLikeHeader): leading spaces and
// markup, invisible characters, case and lookalike characters are ignored.
func quoteHeaders(text string) string {
	// Every character a renderer or model might take as a line break.
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n", "\u0085", "\n", "\v", "\n", "\f", "\n").Replace(text)
	lines := strings.Split(text, "\n")
	tag := []rune("[" + strings.ToLower(version.Slug))
	for i, l := range lines {
		if looksLikeHeader(l, tag) {
			lines[i] = "(quoted) " + l
		}
	}
	return strings.Join(lines, "\n")
}

// looksLikeHeader reports whether line starts with tag once leading
// spaces and markup are skipped, invisible characters (format, combining
// and control characters, spaces, blank fillers) dropped, and the rest
// folded by headerRune. It errs towards quoting: an ordinary line it
// catches only gains a "(quoted) " prefix.
func looksLikeHeader(line string, tag []rune) bool {
	i, lead := 0, true
	for _, r := range line {
		if lead && strings.ContainsRune(">*_`~#-|", r) {
			continue
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) || blankRunes[r] {
			continue
		}
		lead = false
		if headerRune(r) != tag[i] {
			return false
		}
		if i++; i == len(tag) {
			return true
		}
	}
	return false
}

// blankRunes are letters and symbols that render as nothing.
var blankRunes = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true, 0x180E: true}

// headerRune folds r for looksLikeHeader: fullwidth forms, mathematical,
// circled and accented letters, and the common Cyrillic, Greek, small
// capital, modifier and digit lookalikes of the tag's letters, to the
// lowercase ASCII letter they pass for.
func headerRune(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII
		r -= 0xFEE0
	case r >= 0x1D400 && r <= 0x1D6A3: // mathematical letters: styles of A-Z, a-z
		k := (r - 0x1D400) % 52
		if k >= 26 {
			k -= 26
		}
		return 'a' + k
	case r >= 0x24B6 && r <= 0x24CF: // circled capitals
		return 'a' + r - 0x24B6
	case r >= 0x24D0 && r <= 0x24E9: // circled small letters
		return 'a' + r - 0x24D0
	}
	r = unicode.ToLower(r)
	if f, ok := lookalikes[r]; ok {
		return f
	}
	return r
}

// lookalikes maps characters that pass for the letters of the header tag,
// and for its bracket, once lowercased.
var lookalikes = func() map[rune]rune {
	m := map[rune]rune{}
	for to, from := range map[rune]string{
		'[': "\u27e6\u3014\u3010\u3016\u3018\u301a\u2045\u2772\u298b\u298d\u298f\ufe5d\u2e22\u2308\uff3b",
		'c': "\u0441\u03f2\u1d04\u1d9c\u0107\u0109\u010b\u010d\u00e7",
		'h': "\u04bb\u043d\u029c\u02b0\u03b7\u0125\u0127",
		'i': "\u0456\u03b9\u026a\u2071\u0131\u04cf\u00ed\u00ec\u00ee\u00ef\u0129\u012b\u012d\u012fl1",
		'k': "\u043a\u03ba\u1d0b\u1d4f\u0137\u0138",
		'o': "\u043e\u03bf\u1d0f\u1d52\u00f2\u00f3\u00f4\u00f5\u00f6\u00f8\u014d\u014f\u01510",
		'r': "\u0433\u0280\u02b3\u0155\u0157\u0159",
		's': "\u0455\ua731\u02e2\u017f\u015b\u015d\u015f\u0161\u0219",
		't': "\u0442\u03c4\u1d1b\u1d57\u0163\u0165\u0167\u021b",
		'w': "\u051d\u03c9\u1d21\u02b7\u0175",
	} {
		for _, r := range from {
			m[r] = to
		}
	}
	return m
}()

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
		return filesLine(m.Files) + m.Text
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
	if m.Quiet {
		b.WriteString(" quiet=1")
	}
	b.WriteString("]\n")
	if m.Quiet {
		b.WriteString("(Quiet: for your information; it needs no answer, so do not reply just to acknowledge it.)\n")
	}
	switch kind {
	case model.MsgAsk:
		fmt.Fprintf(&b, "(%s is waiting for an answer: reply with the fleet_reply tool, message_id=%s.)\n", m.From, m.ID)
	case model.MsgReply:
		if m.ReplyToText == "" {
			fmt.Fprintf(&b, "(This answers your earlier fleet_ask %s.)\n", m.ReplyTo)
			break
		}
		fmt.Fprintf(&b, "(This answers your earlier fleet_ask %s, which asked:\n", m.ReplyTo)
		for _, l := range strings.Split(strings.TrimRight(m.ReplyToText, "\n"), "\n") {
			b.WriteString("> " + l + "\n")
		}
		b.WriteString(")\n")
	}
	if n := len(m.Attachments); n > 0 {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		fmt.Fprintf(&b, "(%d image%s attached, in this turn's input: %s.)\n", n, plural, attach.Summary(m.Attachments))
	}
	b.WriteString(filesLine(m.Files))
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

// acceptedLocked reports whether the message id was accepted already, and
// forgets ids past their time.
func (s *session) acceptedLocked(id string, now time.Time) bool {
	found := false
	keep := s.rec.Accepted[:0:0] // a copy: saveState may be reading the old one
	for _, a := range s.rec.Accepted {
		if now.After(a.Until) {
			continue
		}
		keep = append(keep, a)
		found = found || a.ID == id
	}
	s.rec.Accepted = keep
	return found
}

// acceptLocked keeps the message id until a redelivery of it can no
// longer come (see seenFor).
func (s *session) acceptLocked(id string, expires, now time.Time) {
	until := now.Add(seenFor)
	if !expires.IsZero() {
		until = expires
		if until.Before(now) {
			until = now
		}
		until = until.Add(seenSlack)
	}
	s.rec.Accepted = append(s.rec.Accepted, acceptedID{ID: id, Until: until})
	if len(s.rec.Accepted) > seenCap {
		s.rec.Accepted = s.rec.Accepted[len(s.rec.Accepted)-seenCap:]
	}
}

// unacceptLocked forgets the message id: the message was not taken after
// all, and a redelivery is welcome.
func (s *session) unacceptLocked(id string) {
	keep := s.rec.Accepted[:0:0]
	for _, a := range s.rec.Accepted {
		if a.ID != id {
			keep = append(keep, a)
		}
	}
	s.rec.Accepted = keep
}

// queuedLocked returns the index of the queued message id, or -1.
func (s *session) queuedLocked(id string) int {
	for i, q := range s.rec.Queue {
		if q.ID == id {
			return i
		}
	}
	return -1
}

// roomLocked refuses m when the session's queue is full for its sender
// (see maxQueueMessages).
func (s *session) roomLocked(m queuedMsg) error {
	count, text := int64(len(s.rec.Queue)), int64(0)
	for _, q := range s.rec.Queue {
		text += int64(len(q.Text))
	}
	if maxN, maxT := share(m, maxQueueMessages), share(m, maxQueueText); count+1 > maxN || text+int64(len(m.Text)) > maxT {
		return fmt.Errorf("the queue of %s is full (%d messages, %d KiB of text waiting; at most %d and %d KiB for this sender); try again once the session has worked through them",
			s.rec.Path, count, text>>10, maxN, maxT>>10)
	}
	return nil
}

// imageRoom refuses m when the images queued on this node, with m's, would
// pass maxQueuedImageBytes for its sender. Called with n.imgMu held.
func (n *Node) imageRoom(m queuedMsg) error {
	var total int64
	for _, a := range m.Attachments {
		total += a.Size
	}
	if total == 0 {
		return nil
	}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		for _, q := range s.rec.Queue {
			for _, a := range q.Attachments {
				total += a.Size
			}
		}
		s.mu.Unlock()
	}
	if limit := share(m, maxQueuedImageBytes); total > limit {
		return fmt.Errorf("%s has too many images queued for its sessions (%d MiB with these; at most %d MiB for this sender); try again once they are delivered",
			n.cfg.Host, total>>20, limit>>20)
	}
	return nil
}

// deliver accepts a message into a session. Redeliveries of a message it
// has already accepted are acknowledged without effect. Steering falls back
// to queueing when there is no steerable turn, and auto steers only when a
// turn is running; interrupt puts the message at the head of its part of
// the queue and cancels the running turn. A message the node cannot keep
// (a full queue, or state.json not saved) is refused, so its sender hears.
func (n *Node) deliver(ctx context.Context, s *session, m *wire.MessageCmd) (json.RawMessage, error) {
	if m == nil || strings.TrimSpace(m.Text) == "" {
		return nil, errors.New("deliver: empty message")
	}
	if err := checkMessage(m); err != nil {
		return nil, err
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
	files, err := attach.NormalizeFiles(m.Files)
	if err != nil {
		return nil, err
	}
	msg := queuedMsg{ID: m.ID, From: m.From, Via: m.Via, Owner: m.Owner, Text: m.Text, Kind: m.Kind, ReplyTo: m.ReplyTo, ReplyToText: m.ReplyToText,
		Attachments: model.MetaOnly(images), ExpiresAt: m.ExpiresAt, Quiet: m.Quiet && m.Mode != model.ModeInterrupt}
	caps := h.Caps()
	steer := caps.Steer && (m.Mode == model.ModeSteer || (m.Mode == model.ModeAuto && s.turnRunning(h)))
	if steer && m.Mode == model.ModeAuto && s.turnRunning(h) && strings.HasPrefix(m.From, "user:") {
		// A person's message sent Now goes into the running turn at its
		// next step, from the queue, where they can still cancel it until
		// then (stepSteers). An explicit steer, and agents', go in at once.
		steer, msg.AtStep = false, true
	}
	s.mu.Lock()
	if s.acceptedLocked(m.ID, time.Now()) || s.queuedLocked(m.ID) >= 0 {
		s.mu.Unlock()
		return model.Marshal(wire.DeliverResult{Mode: model.ModeQueue, Duplicate: true}), nil
	}
	if !steer {
		// Refused before its files are written; checked again as it is
		// queued.
		if err := s.roomLocked(msg); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.acceptLocked(m.ID, m.ExpiresAt, time.Now())
	s.mu.Unlock()
	unseen := func() {
		s.mu.Lock()
		s.unacceptLocked(m.ID)
		s.mu.Unlock()
	}
	if len(files) > 0 {
		if msg.Files, err = n.writeFiles(s.rec.ID, m.ID, files); err != nil {
			unseen()
			return nil, fmt.Errorf("write files: %w", err)
		}
	}
	if steer {
		// Its id is saved before it goes in, so a redelivery after a crash
		// is a duplicate rather than a second steer.
		if err := n.saveState(); err != nil {
			unseen()
			return nil, fmt.Errorf("save node state: %w", err)
		}
		err := n.steer(s, h, msg, images)
		if err == nil {
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer}), nil // flushSteers reported it delivered
		}
		if errors.Is(err, errUnknown) {
			// Not queued as well, which could deliver it twice; the
			// session's log has a warning.
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer}), nil
		}
		if errors.Is(err, errExpired) {
			// Its id stays among the accepted ones, so a redelivery does
			// not bring it back.
			s.emit(model.KindMessageExpired, model.MessageEvent{MessageID: msg.ID, From: msg.From, Error: err.Error()})
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer, Expired: true}), nil
		}
		if errors.Is(err, adapter.ErrNoImages) {
			unseen()
			n.persist()
			return nil, err
		}
		if m.Mode == model.ModeSteer || !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: steer failed (%v); queueing instead", s.rec.Path, err)
		}
	}
	// n.imgMu orders the node's image limit against other messages'
	// images; s.imgMu keeps gcImages from removing these files before the
	// message is in the queue.
	n.imgMu.Lock()
	if err := n.imageRoom(msg); err != nil {
		n.imgMu.Unlock()
		unseen()
		return nil, err
	}
	s.imgMu.Lock()
	if _, err := n.storeImages(s.rec.ID, images); err != nil {
		s.imgMu.Unlock()
		n.imgMu.Unlock()
		unseen()
		return nil, fmt.Errorf("store images: %w", err)
	}
	interrupt := m.Mode == model.ModeInterrupt && caps.Interrupt
	s.mu.Lock()
	if err := s.roomLocked(msg); err != nil {
		s.unacceptLocked(m.ID)
		s.mu.Unlock()
		s.imgMu.Unlock()
		n.imgMu.Unlock()
		if len(images) > 0 {
			n.gcImages(s)
		}
		return nil, err
	}
	s.rec.Queue = enqueue(s.rec.Queue, msg, interrupt)
	s.reportQueueLocked()
	s.mu.Unlock()
	s.imgMu.Unlock()
	n.imgMu.Unlock()
	if err := n.saveState(); err != nil {
		// Not kept: refused, so the sender hears, unless a delivery has
		// taken it meanwhile.
		s.mu.Lock()
		i := s.queuedLocked(msg.ID)
		taken := i >= 0 && s.rec.Queue[i].Sending
		if i >= 0 && !taken {
			s.rec.Queue = without(s.rec.Queue, []queuedMsg{msg})
			s.unacceptLocked(msg.ID)
			s.reportQueueLocked()
		}
		s.mu.Unlock()
		if !taken {
			n.wakeSender()
			if len(images) > 0 {
				n.gcImages(s)
			}
			return nil, fmt.Errorf("save node state: %w", err)
		}
		n.logf("session %s: save node state: %v", s.rec.Path, err)
	}
	mode := model.ModeQueue
	if interrupt {
		mode = model.ModeInterrupt
	}
	queued := mode
	if msg.AtStep {
		queued = model.ModeSteer
	}
	s.emit(model.KindMessageQueued, model.MessageEvent{MessageID: msg.ID, From: msg.From, Text: msg.Text, Mode: queued, Attachments: msg.Attachments})
	if interrupt {
		if err := n.interrupt(ctx, s, h); err != nil && !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: interrupt for message %s: %v", s.rec.Path, msg.ID, err)
		}
	}
	go n.tryDeliver(s)
	return model.Marshal(wire.DeliverResult{Mode: mode}), nil
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
	// How long a steer, and a new turn, may take to be accepted before its
	// outcome counts as unknown (errUnknown).
	steerTimeout = 30 * time.Second
	turnTimeout  = 60 * time.Second
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

// sweepQueues drops expired messages from every session's queue, whatever
// the session is doing (a long turn or an approval wait would otherwise
// keep them), and retries delivery for sessions that have stayed idle with
// queued messages since the last sweep, logging a warning for each: with
// a working harness that never lasts more than a moment.
func (n *Node) sweepQueues(now time.Time) {
	for _, s := range n.sessionList() {
		s.mu.Lock()
		var expired []queuedMsg
		if !s.rec.Stopped {
			expired = s.dropExpiredLocked(now)
		}
		s.mu.Unlock()
		if len(expired) > 0 {
			n.wakeSender()
			n.persist()
			n.gcFor(s, expired)
		}
		s.mu.Lock()
		stuck := !s.rec.Stopped && s.h != nil && !s.delivering && s.waitingLocked() && s.h.Status() == model.StatusIdle
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
		// Messages that expired while they waited are dropped, never
		// steered in.
		now := time.Now()
		var expired []*steerReq
		live := s.steerBuf[:0:0]
		for _, r := range s.steerBuf {
			if r.msg.expired(now) {
				expired = append(expired, r)
			} else {
				live = append(live, r)
			}
		}
		s.steerBuf = live
		msgs := make([]queuedMsg, len(s.steerBuf))
		for i, r := range s.steerBuf {
			msgs[i] = r.msg
		}
		k := batchLen(msgs)
		if k == 0 {
			s.steering = false
			s.mu.Unlock()
			for _, r := range expired {
				r.done <- errExpired
			}
			return
		}
		batch := s.steerBuf[:k:k]
		s.steerBuf = s.steerBuf[k:]
		// The turn they go into: the one running now. Read after Deliver,
		// it could be a later one, or none.
		into := s.activeTurn
		s.mu.Unlock()
		for _, r := range expired {
			r.done <- errExpired
		}
		var images []model.Attachment
		for _, r := range batch {
			images = append(images, r.images...)
		}
		ctx, cancel := context.WithTimeout(context.Background(), steerTimeout)
		err := h.Deliver(ctx, batchInput(msgs[:k], images), adapter.DeliverSteer)
		if timedOut(ctx, err) {
			err = errUnknown
		}
		cancel()
		if err == nil {
			// Reported here, with the turn they went into, before that
			// turn's completion can be: message.seen never comes first.
			s.mu.Lock()
			s.flushDeltaLocked()
			ids := make([]string, len(batch))
			for i, r := range batch {
				ids[i] = r.msg.ID
				s.appendLocked(model.KindMessageDelivered, model.MessageEvent{MessageID: r.msg.ID, From: r.msg.From, Text: r.msg.Text,
					Mode: model.ModeSteer, Attachments: r.msg.Attachments})
			}
			s.steeredLocked(into, ids)
			s.mu.Unlock()
			n.wakeSender()
		}
		if errors.Is(err, errUnknown) {
			s.emit(model.KindWarning, model.ErrorEvent{Message: fmt.Sprintf("%d steered message(s) may or may not have reached the session: %v", len(batch), err)})
		}
		for _, r := range batch {
			r.done <- err
		}
	}
}

// errUnknown answers a delivery the harness did not answer in time: it may
// still have taken it (Codex can start the turn after the node stopped
// waiting), so it is neither resent nor reported failed.
var errUnknown = errors.New("the harness did not answer in time; the message may or may not have gone in, so it is not sent again")

// timedOut reports whether a delivery failed because its context ran out.
func timedOut(ctx context.Context, err error) bool {
	return err != nil && !errors.Is(err, adapter.ErrBusy) && !errors.Is(err, adapter.ErrNotActive) &&
		(errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil)
}

// nextBatchLocked picks the queued messages the next turn carries: those
// not being sent already, within batchLen, and only one if it is one that
// failed in a batch (solo) or the batch would reach one. Quiet messages
// never start a turn: with only those waiting there is no batch, and a
// batch always holds a message that is not quiet (moved to the front if
// the bounds would leave it out).
func (s *session) nextBatchLocked() []queuedMsg {
	var cands []queuedMsg
	lead := -1 // the first message that may start a turn
	for _, m := range s.rec.Queue {
		if !m.Sending {
			if lead < 0 && m.wakes() {
				lead = len(cands)
			}
			cands = append(cands, m)
		}
	}
	if lead < 0 {
		return nil
	}
	if s.solo[cands[0].ID] {
		// Retried alone even if quiet: it was in a batch a message that is
		// not quiet started.
		return cands[:1]
	}
	if lead >= batchLen(cands) {
		cands = append([]queuedMsg{cands[lead]}, append(cands[:lead:lead], cands[lead+1:]...)...)
	}
	k := batchLen(cands)
	for i := 1; i < k; i++ {
		if s.solo[cands[i].ID] {
			k = i
			break
		}
	}
	return cands[:k]
}

// sendingLocked reports whether a queued message is being handed to the
// harness.
func (s *session) sendingLocked() bool {
	for _, m := range s.rec.Queue {
		if m.Sending {
			return true
		}
	}
	return false
}

// waitingLocked reports whether a queued message would start a turn: one
// that is not quiet, or being sent.
func (s *session) waitingLocked() bool {
	for _, m := range s.rec.Queue {
		if m.Sending || m.wakes() {
			return true
		}
	}
	return false
}

// markLocked sets or clears Sending on the queued messages of batch. The
// queue is copied, not changed in place: saveState may be reading it.
func (s *session) markLocked(batch []queuedMsg, sending bool) {
	in := map[string]bool{}
	for _, m := range batch {
		in[m.ID] = true
	}
	q := append([]queuedMsg(nil), s.rec.Queue...)
	for i := range q {
		if in[q[i].ID] {
			q[i].Sending = sending
		}
	}
	s.rec.Queue = q
}

// leftLocked takes the messages of batch out of the queue, for good.
func (s *session) leftLocked(batch []queuedMsg) {
	s.rec.Queue = without(s.rec.Queue, batch)
	for _, m := range batch {
		delete(s.solo, m.ID)
	}
	s.reportQueueLocked()
}

// failedMsg is a queued message that could not be delivered, and why.
type failedMsg struct {
	m   queuedMsg
	err error
}

// tryDeliver starts a turn with the next queued messages if the session is
// idle. The batch is marked as being sent, and saved, before the harness
// gets it, and reported delivered once it has gone in. If the harness
// refuses a batch of several, each is retried on its own, so only a
// message that fails alone is reported failed; one whose images are gone
// fails before anything is sent.
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
		if expired := s.dropExpiredLocked(time.Now()); len(expired) > 0 {
			// Dropped, never delivered: no turn starts for them. Look again,
			// for the rest of the queue or to settle.
			s.mu.Unlock()
			n.wakeSender()
			n.persist()
			n.gcFor(s, expired)
			continue
		}
		batch := s.nextBatchLocked()
		if len(batch) == 0 {
			if s.sendingLocked() {
				s.mu.Unlock()
				return // a promoted message is being steered in
			}
			s.solo = nil
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
		s.markLocked(batch, true)
		h := s.h
		id := s.rec.ID
		s.mu.Unlock()
		// Saved before the harness gets them: after a crash from here on,
		// a restart reports them failed rather than sending them twice.
		n.persist()

		var failed []failedMsg
		var good []queuedMsg
		var images []model.Attachment
		for _, m := range batch {
			im, err := n.loadImages(id, m.Attachments)
			if err != nil {
				failed = append(failed, failedMsg{m, err})
				continue
			}
			good = append(good, m)
			images = append(images, im...)
		}
		s.mu.Lock()
		s.nextTurn, s.announce = nil, good
		for _, m := range good {
			s.nextTurn = append(s.nextTurn, m.ID) // for the turn this starts
		}
		s.mu.Unlock()
		var err error
		unknown := false
		if len(good) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
			err = h.Deliver(ctx, batchInput(good, images), adapter.DeliverQueue)
			unknown = timedOut(ctx, err)
			cancel()
		}

		s.mu.Lock()
		s.delivering = false
		var left []queuedMsg // out of the queue for good
		again := false
		var wait time.Duration
		switch {
		case len(good) == 0:
			s.nextTurn, s.announce = nil, nil
		case errors.Is(err, adapter.ErrBusy):
			// Not delivered after all: they stay at the head of the queue and
			// are retried on the next idle status, at once if one came in
			// meanwhile, or after a backoff if the session says it is idle
			// already (no such status may come).
			s.nextTurn, s.announce = nil, nil
			s.markLocked(good, false)
			again = s.redeliver
			s.redeliver = false
			if !again && s.rec.Status == model.StatusIdle {
				wait = busyBackoff[min(s.busyTries, len(busyBackoff)-1)]
				s.busyTries++
			}
		case unknown:
			// The harness may still start the turn: they leave the queue
			// neither resent nor failed. If the turn does start,
			// turn.started reports them delivered and its completion seen.
			s.busyTries = 0
			s.flushDeltaLocked()
			s.appendLocked(model.KindWarning, model.ErrorEvent{Message: fmt.Sprintf("%d queued message(s) may or may not have reached the session: %v", len(good), errUnknown)})
			left = good
		case err != nil && len(good) > 1:
			// One bad message must not fail the others: each is tried
			// again on its own.
			s.nextTurn, s.announce = nil, nil
			s.markLocked(good, false)
			if s.solo == nil {
				s.solo = map[string]bool{}
			}
			for _, m := range good {
				s.solo[m.ID] = true
			}
			n.logf("session %s: delivering %d messages together failed (%v); trying each on its own", s.rec.Path, len(good), err)
			again = true
		case err != nil:
			s.nextTurn, s.announce = nil, nil
			failed = append(failed, failedMsg{good[0], err})
		default:
			s.busyTries = 0
			s.announceLocked() // unless turn.started did already
			left = good
		}
		if len(failed) > 0 && !s.rec.Stopped { // stop reported the queue failed
			s.flushDeltaLocked()
			for _, f := range failed {
				s.appendLocked(model.KindMessageFailed, model.MessageEvent{MessageID: f.m.ID, From: f.m.From, Error: f.err.Error()})
				left = append(left, f.m)
			}
		}
		s.leftLocked(left)
		path, tries := s.rec.Path, s.busyTries
		s.mu.Unlock()
		n.wakeSender()
		n.persist()
		n.gcFor(s, left)
		if errors.Is(err, adapter.ErrBusy) && !again {
			if wait > 0 {
				n.logf("session %s: harness busy although idle (attempt %d); retrying delivery in %s", path, tries, wait)
				n.retryBusy(s, wait)
			}
			return
		}
		// Loop: if the turn already finished (or failed to start), the next
		// queued messages go now; otherwise the idle status triggers it.
	}
}

// gcFor removes stored images no queued message needs any more, if any of
// msgs (which left the queue) had some.
func (n *Node) gcFor(s *session, msgs []queuedMsg) {
	for _, m := range msgs {
		if len(m.Attachments) > 0 {
			n.gcImages(s)
			return
		}
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

// dropExpiredLocked takes the messages past their expiry out of the queue,
// reporting each as expired (message.expired), and returns them.
func (s *session) dropExpiredLocked(now time.Time) []queuedMsg {
	var expired []queuedMsg
	for _, m := range s.rec.Queue {
		if m.expired(now) && !m.Sending { // one being sent is past expiring
			expired = append(expired, m)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	s.flushDeltaLocked()
	for _, m := range expired {
		s.appendLocked(model.KindMessageExpired, model.MessageEvent{MessageID: m.ID, From: m.From, Error: "expired in the queue before it was delivered"})
	}
	s.leftLocked(expired)
	return expired
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

// queueList lists a session's queue (OpQueue).
func (n *Node) queueList(s *session) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := wire.QueueResult{Messages: []wire.QueueEntry{}}
	for i, m := range s.rec.Queue {
		res.Messages = append(res.Messages, wire.QueueEntry{ID: m.ID, From: m.From, Kind: m.Kind, Owner: m.Owner,
			Images: len(m.Attachments), Position: i, ExpiresAt: m.ExpiresAt, AtStep: m.AtStep, Quiet: m.Quiet})
	}
	return model.Marshal(res)
}

// takeQueued removes a queued message to cancel it. It refuses one that is
// not queued, or that is being handed to the harness.
func (s *session) takeQueued(id string) (queuedMsg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.queuedLocked(id)
	if i < 0 {
		return queuedMsg{}, fmt.Errorf("message %s is not queued in %s (delivered already?)", id, s.rec.Path)
	}
	m := s.rec.Queue[i]
	if m.Sending {
		return queuedMsg{}, fmt.Errorf("message %s is being delivered now", id)
	}
	s.leftLocked([]queuedMsg{m})
	return m, nil
}

// promoteLocked readies the queued message id to go now: its expiry is
// cleared and, to be steered in, it is marked as being sent (it stays in
// the queue meanwhile, so its images stay and a save keeps it), or else
// moved to the very head of the queue, to go next.
func (s *session) promoteLocked(id string, steer bool) (queuedMsg, error) {
	i := s.queuedLocked(id)
	if i < 0 {
		return queuedMsg{}, fmt.Errorf("message %s is not queued in %s (delivered already?)", id, s.rec.Path)
	}
	m := s.rec.Queue[i]
	if m.Sending {
		return queuedMsg{}, fmt.Errorf("message %s is being delivered now", id)
	}
	// A person asked for it now, which outranks its expiry: it goes even
	// if it would have expired before its turn came.
	m.ExpiresAt = time.Time{}
	m.Sending = steer
	m.Promoted = true // asked for now: it starts a turn even if quiet
	if steer {
		q := append([]queuedMsg(nil), s.rec.Queue...)
		q[i] = m
		s.rec.Queue = q
	} else {
		s.rec.Queue = append([]queuedMsg{m}, append(s.rec.Queue[:i:i], s.rec.Queue[i+1:]...)...)
	}
	return m, nil
}

// promote delivers a queued message now (OpPromote): steered into the
// running turn, or with interrupt, first in the queue with the running turn
// cancelled. A steer that finds no running turn leaves it first in the
// queue, so it goes next.
func (n *Node) promote(ctx context.Context, s *session, c *wire.QueuedCmd) (json.RawMessage, error) {
	if c == nil || c.MessageID == "" {
		return nil, errors.New("promote: missing message id")
	}
	if c.Mode != model.ModeSteer && c.Mode != model.ModeInterrupt {
		return nil, fmt.Errorf("promote: mode %q (steer or interrupt)", c.Mode)
	}
	h, err := s.handle()
	if err != nil {
		return nil, err
	}
	caps := h.Caps()
	if c.Mode == model.ModeInterrupt && !caps.Interrupt {
		return nil, fmt.Errorf("%s sessions cannot be interrupted", s.rec.Harness)
	}
	steer := c.Mode == model.ModeSteer && caps.Steer && s.turnRunning(h)
	s.mu.Lock()
	msg, err := s.promoteLocked(c.MessageID, steer)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if steer {
		// Saved as being sent first, so a crash cannot deliver it twice.
		n.persist()
		images, err := n.loadImages(s.rec.ID, msg.Attachments)
		if err == nil {
			err = n.steer(s, h, msg, images)
		}
		if err == nil || errors.Is(err, errUnknown) {
			s.mu.Lock()
			s.leftLocked([]queuedMsg{msg})
			s.mu.Unlock()
			n.persist()
			if err == nil {
				// flushSteers reported it delivered.
				s.emit(model.KindMessagePromoted, model.MessageEvent{MessageID: msg.ID, From: msg.From, Mode: model.ModeSteer})
			}
			n.gcFor(s, []queuedMsg{msg})
			if err != nil {
				return nil, err
			}
			return model.Marshal(wire.DeliverResult{Mode: model.ModeSteer}), nil
		}
		n.logf("session %s: steer of promoted message %s failed (%v); it goes next", s.rec.Path, msg.ID, err)
		s.mu.Lock()
		if s.queuedLocked(msg.ID) >= 0 { // stop may have failed the queue meanwhile
			s.markLocked([]queuedMsg{msg}, false)
			_, _ = s.promoteLocked(msg.ID, false)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.reportQueueLocked()
	s.mu.Unlock()
	n.persist()
	mode := model.ModeQueue
	if c.Mode == model.ModeInterrupt {
		mode = model.ModeInterrupt
	}
	s.emit(model.KindMessagePromoted, model.MessageEvent{MessageID: msg.ID, From: msg.From, Mode: mode})
	if mode == model.ModeInterrupt {
		if err := n.interrupt(ctx, s, h); err != nil && !errors.Is(err, adapter.ErrNotActive) {
			n.logf("session %s: interrupt for message %s: %v", s.rec.Path, msg.ID, err)
		}
	}
	go n.tryDeliver(s)
	return model.Marshal(wire.DeliverResult{Mode: mode}), nil
}

// cancelQueued drops a queued message (OpCancel), and names the message
// that superseded it, if one did. Its id stays among the accepted ones, so
// a redelivery from the coordinator does not bring it back.
func (n *Node) cancelQueued(s *session, c *wire.QueuedCmd) error {
	if c == nil || c.MessageID == "" {
		return errors.New("cancel: missing message id")
	}
	msg, err := s.takeQueued(c.MessageID)
	if err != nil {
		return err
	}
	n.persist()
	s.emit(model.KindMessageCancelled, model.MessageEvent{MessageID: msg.ID, From: msg.From, SupersededBy: c.SupersededBy})
	n.gcFor(s, []queuedMsg{msg})
	go n.tryDeliver(s) // the session may have settled
	return nil
}

// stop stops a session. Its queued messages are reported failed, so their
// senders hear, and the node forgets the session once the coordinator has
// all its events (forgetStopped).
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
	s.failQueueLocked("the session was stopped before it was delivered")
	id := s.rec.ID
	s.mu.Unlock()
	n.wakeSender()
	n.dropImages(id)
	return n.saveState()
}

// failQueueLocked reports every queued message failed and empties the
// queue, for a session that will not run again.
func (s *session) failQueueLocked(why string) {
	if len(s.rec.Queue) > 0 {
		s.flushDeltaLocked()
		for _, m := range s.rec.Queue {
			s.appendLocked(model.KindMessageFailed, model.MessageEvent{MessageID: m.ID, From: m.From, Error: why})
		}
	}
	s.rec.Queue, s.rec.Accepted, s.solo, s.announce = nil, nil, nil, nil
	s.reportQueueLocked()
}

// recover readies a record loaded from state.json, and reports whether it
// changed it: ids an older node kept are converted, messages a crash left
// marked as being sent are reported failed rather than sent a second time
// (see queuedMsg.Sending: delivery is at most once), and a stopped
// session's queue (older nodes kept it) is failed.
func (s *session) recover(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, id := range s.rec.Seen {
		s.rec.Accepted = append(s.rec.Accepted, acceptedID{ID: id, Until: now.Add(seenFor)})
		changed = true
	}
	s.rec.Seen = nil
	if s.rec.Stopped {
		if len(s.rec.Queue) > 0 || len(s.rec.Accepted) > 0 {
			s.failQueueLocked("the session was stopped before it was delivered")
			changed = true
		}
		return changed
	}
	var lost []queuedMsg
	for _, m := range s.rec.Queue {
		if m.Sending {
			lost = append(lost, m)
		}
	}
	if len(lost) > 0 {
		s.flushDeltaLocked()
		for _, m := range lost {
			s.appendLocked(model.KindMessageFailed, model.MessageEvent{MessageID: m.ID, From: m.From,
				Error: "the node restarted while handing it to the session; it may or may not have arrived, so it is not sent again"})
		}
		s.leftLocked(lost)
		changed = true
	}
	return changed
}

// forgetStopped drops stopped sessions whose events the coordinator has
// all: their record, spool and attachments. A command for one is then
// answered as for a stopped session.
func (n *Node) forgetStopped() {
	var gone []*session
	n.mu.Lock()
	for id, s := range n.sessions {
		s.mu.Lock()
		if s.rec.Stopped && len(s.unacked) == 0 && s.delta == nil && s.localReqs == 0 && !s.delivering && !s.steering {
			if s.spool != nil {
				_ = s.spool.Close()
				s.spool = nil
			}
			gone = append(gone, s)
			delete(n.sessions, id)
			if n.forgotten == nil {
				n.forgotten = map[string]bool{}
			}
			n.forgotten[id] = true
		}
		s.mu.Unlock()
	}
	n.mu.Unlock()
	if len(gone) == 0 {
		return
	}
	for _, s := range gone {
		_ = os.Remove(s.spoolPath())
		n.dropImages(s.rec.ID)
		n.logf("session %s: stopped, and its events are all with the coordinator; forgotten", s.rec.Path)
	}
	n.persist()
}

// setPolicy changes a running session's approval policy and keeps it for
// a resume.
func (n *Node) setPolicy(ctx context.Context, s *session, policy string) error {
	switch policy {
	case "never", "on-request", "on-failure", "untrusted":
	default:
		return fmt.Errorf("unknown approval policy %q (never, on-request, on-failure, untrusted)", policy)
	}
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
			if cmd.Op == wire.OpStop && errors.Is(err, errForgotten) {
				return nil // stopped already
			}
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
			return n.interrupt(ctx, s, h)
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
		case wire.OpQueue:
			data = n.queueList(s)
			return nil
		case wire.OpPromote:
			var err error
			data, err = n.promote(ctx, s, cmd.Queued)
			return err
		case wire.OpCancel:
			return n.cancelQueued(s, cmd.Queued)
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
	if n.filesRoot() != "" {
		h.Features = append(h.Features, wire.FeatureFiles)
	}
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
