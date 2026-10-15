// SPDX-License-Identifier: GPL-3.0-or-later

// Package agyad adapts the Antigravity CLI (`agy`) to the adapter interface.
//
// EXPERIMENTAL: built from agy's documented headless mode
// (https://antigravity.google/docs/cli/headless) and tested against a fake
// that emits exactly the documented events; not yet run against a real agy.
//
// By default each session runs one long-lived
//
//	agy --output-format stream-json --input-format stream-json [--conversation ID]
//
// process. Prompts are written to its stdin as {"event":"user",...} lines;
// every turn ends with its own "result" event. With Options.PerTurn, each
// turn runs its own process (prompt on stdin, stdin then closed), which is
// slower but uses only the older single-shot mode.
//
// stream-json output is NDJSON whose "event" field is init, step_update or
// result, with the payload under the key of the same name. step_update
// carries text_delta for agent text and tool_info for tool steps; result
// carries status, response, conversation_id and usage (cumulative for the
// process in stream-input mode).
//
// Differences from Codex: no approval relay (headless agy never asks; tools
// that would need confirmation are soft-denied unless allowed in agy's
// settings or the session runs with danger-full-access), no mid-turn
// steering, and Interrupt stops the process (the next turn starts a new one
// on the same conversation). The conversation id is only known once agy
// reports it; session.registered is emitted again then.
//
// Fleet tools (Options.FleetTools, opt-in): agy has no per-run MCP flag, so
// the node adds a "swyd" entry to the workspace's .agents/mcp_config.json
// (command and args only; the per-session credentials reach `swyd mcp`
// through the agy process environment) and, unless the session already
// skips permissions, an "mcp(swyd/*)" allow rule to agy's settings.json.
package agyad

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/jsonrpc"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/proc"
	"github.com/jm2/switchyard/internal/ulid"
)

// Harness is this adapter's name.
const Harness = "agy"

// Options adjust agy behavior (node.json "agy" section).
type Options struct {
	// PerTurn runs one agy process per turn instead of one long-lived
	// --input-format stream-json process per session.
	PerTurn bool `json:"per_turn,omitempty"`
	// FleetTools gives agy sessions the fleet tools through the
	// workspace's .agents/mcp_config.json (see the package comment).
	FleetTools bool `json:"fleet_tools,omitempty"`
	// SettingsPath overrides ~/.gemini/antigravity-cli/settings.json, where
	// the fleet tools' allow rule is added.
	SettingsPath string `json:"settings_path,omitempty"`
}

// Config configures the adapter.
type Config struct {
	Bin       string            // default "agy"
	ExtraArgs []string          // appended to every invocation
	Env       map[string]string // environment overrides
	Options   Options
	Stderr    io.Writer
	Tap       jsonrpc.Tap // raw stream capture: "out" = argv and stdin lines, "in" = stdout lines
	Logf      func(format string, args ...any)
}

// Adapter runs agy sessions.
type Adapter struct {
	cfg Config
	mu  sync.Mutex
	hs  map[*handle]bool
}

// New returns an adapter.
func New(cfg Config) *Adapter {
	if cfg.Bin == "" {
		cfg.Bin = "agy"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Adapter{cfg: cfg, hs: map[*handle]bool{}}
}

func (a *Adapter) Name() string { return Harness }

// Start creates a session. agy assigns the conversation id on the first turn.
func (a *Adapter) Start(ctx context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	return a.open(spec, sink, "")
}

// Resume reattaches to a conversation by id. The process starts with the
// next turn.
func (a *Adapter) Resume(ctx context.Context, ref model.NativeRef, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if ref.Harness != "" && ref.Harness != Harness {
		return nil, fmt.Errorf("agy adapter cannot resume %s session", ref.Harness)
	}
	return a.open(spec, sink, ref.ID)
}

func (a *Adapter) open(spec adapter.StartSpec, sink adapter.Sink, convID string) (adapter.Handle, error) {
	if _, err := proc.LookPath(a.cfg.Bin); err != nil {
		return nil, err
	}
	h := &handle{a: a, spec: spec, sink: sink, convID: convID, status: model.StatusStarting}
	a.mu.Lock()
	a.hs[h] = true
	a.mu.Unlock()
	if spec.MCP != nil && a.cfg.Options.FleetTools {
		for _, w := range setupFleetTools(spec, a.cfg) {
			a.cfg.Logf("agy: %s", w)
			h.emit(model.KindWarning, model.ErrorEvent{Message: w})
		}
	}
	h.emitRegistered(convID != "")
	h.setStatus(model.StatusIdle, "")
	return h, nil
}

// Close stops every agy process.
func (a *Adapter) Close() error {
	a.mu.Lock()
	hs := make([]*handle, 0, len(a.hs))
	for h := range a.hs {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	for _, h := range hs {
		h.shutdown(false)
	}
	return nil
}

type handle struct {
	a    *Adapter
	spec adapter.StartSpec
	sink adapter.Sink

	mu           sync.Mutex
	convID       string
	status       string
	p            *proc.Process // the running agy process
	turn         *turnState    // the active turn
	stopped      bool
	interrupting bool
	lastRep      *model.Usage // last cumulative usage agy reported
	total        model.Usage
	deniedSeen   map[string]bool
}

func (h *handle) emit(kind string, p any) { h.sink(adapter.Event{Kind: kind, Payload: p}) }

func (h *handle) Ref() model.NativeRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.NativeRef{Harness: Harness, ID: h.convID}
}

func (h *handle) Caps() model.Caps {
	return model.Caps{Message: true, Interrupt: true, Stop: true}
}

func (h *handle) Status() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *handle) setStatus(s, detail string) {
	h.mu.Lock()
	if h.status == s && detail == "" {
		h.mu.Unlock()
		return
	}
	h.status = s
	h.mu.Unlock()
	h.emit(model.KindSessionStatus, model.SessionStatus{Status: s, Detail: detail})
}

func (h *handle) emitRegistered(resumed bool) {
	h.emit(model.KindSessionRegistered, model.SessionRegistered{
		NativeRef: h.Ref(), Cwd: h.spec.Cwd, Model: h.spec.Model, Caps: h.Caps(), Resumed: resumed,
	})
}

func (h *handle) args() []string {
	args := []string{"--output-format", "stream-json"}
	if !h.a.cfg.Options.PerTurn {
		args = append(args, "--input-format", "stream-json")
	}
	if h.spec.Model != "" {
		args = append(args, "--model", h.spec.Model)
	}
	h.mu.Lock()
	if h.convID != "" {
		args = append(args, "--conversation", h.convID)
	}
	h.mu.Unlock()
	switch h.spec.Sandbox {
	case "danger-full-access":
		args = append(args, "--dangerously-skip-permissions")
	case "read-only", "workspace-write":
		args = append(args, "--sandbox")
	}
	return append(args, h.a.cfg.ExtraArgs...)
}

func (h *handle) env() map[string]string {
	env := map[string]string{}
	for k, v := range h.a.cfg.Env {
		env[k] = v
	}
	for k, v := range h.spec.Env {
		env[k] = v
	}
	// agy starts `swyd mcp` from the workspace MCP config; the session's
	// credentials reach it through agy's own environment.
	if h.spec.MCP != nil && h.a.cfg.Options.FleetTools {
		for k, v := range h.spec.MCP.Env {
			env[k] = v
		}
	}
	return env
}

// startProc launches agy for turn t. Callers hold no locks. t.proc is set
// before the reader starts, so a reader only ever treats its own turn as
// ended when its process exits.
func (h *handle) startProc(t *turnState) (*proc.Process, error) {
	args := h.args()
	stderr := h.a.cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	p, err := proc.Start(h.a.cfg.Bin, args, proc.Options{Dir: h.spec.Cwd, Env: proc.Environ(h.env()),
		Stderr: &lineWriter{w: stderr, line: h.stderrLine}})
	if err != nil {
		return nil, fmt.Errorf("start agy: %w", err)
	}
	if tap := h.a.cfg.Tap; tap != nil {
		b, _ := json.Marshal(map[string]any{"argv": append([]string{h.a.cfg.Bin}, args...)})
		tap("out", b)
	}
	h.mu.Lock()
	h.lastRep = nil // agy's cumulative usage restarts with each process
	t.proc = p
	h.mu.Unlock()
	go h.read(p)
	return p, nil
}

// Deliver runs one turn. Only queue delivery is supported.
func (h *handle) Deliver(ctx context.Context, in adapter.Input, mode adapter.DeliveryMode) error {
	if mode == adapter.DeliverSteer {
		return adapter.ErrNotSupported
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return adapter.ErrClosed
	}
	if h.turn != nil {
		h.mu.Unlock()
		return adapter.ErrBusy
	}
	t := &turnState{h: h, id: ulid.New(), start: time.Now(), steps: map[int]*step{}}
	h.turn = t
	p := h.p
	h.mu.Unlock()

	fail := func(err error) error {
		h.mu.Lock()
		h.turn = nil
		h.mu.Unlock()
		return err
	}
	perTurn := h.a.cfg.Options.PerTurn
	if p == nil || perTurn {
		var err error
		if p, err = h.startProc(t); err != nil {
			return fail(err)
		}
		h.mu.Lock()
		h.p = p
		h.mu.Unlock()
	}
	h.mu.Lock()
	t.proc = p // the reused long-lived process (startProc set it otherwise)
	h.mu.Unlock()
	var line []byte
	if perTurn {
		line = []byte(in.Text)
	} else {
		line, _ = json.Marshal(map[string]any{"event": "user", "message": map[string]any{"content": in.Text}})
		line = append(line, '\n')
	}
	if tap := h.a.cfg.Tap; tap != nil {
		tap("out", bytes.TrimRight(line, "\n"))
	}
	userItem := model.Item{TurnID: t.id, ItemID: t.id + "-user", ItemType: "userMessage",
		Raw: model.Marshal(map[string]any{"type": "userMessage", "content": []map[string]string{{"type": "text", "text": in.Text}}})}
	h.emit(model.KindTurnStarted, model.TurnStarted{TurnID: t.id})
	h.emit(model.KindItemStarted, userItem)
	h.emit(model.KindItemCompleted, userItem)
	h.setStatus(model.StatusActive, "")
	// Write before returning so an immediate Interrupt cannot overtake it.
	if _, err := p.Stdin.Write(line); err != nil {
		_ = p.Kill()
		return nil // read() reports the failed turn when the process ends
	}
	if perTurn {
		_ = p.Stdin.Close()
	}
	return nil
}

// read consumes one agy process's stdout until it exits.
func (h *handle) read(p *proc.Process) {
	br := bufio.NewReaderSize(p.Stdout, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if s := bytes.TrimSpace(line); len(s) > 0 {
			if tap := h.a.cfg.Tap; tap != nil {
				tap("in", s)
			}
			h.handleLine(s)
		}
		if err != nil {
			break
		}
	}
	<-p.Done()
	h.mu.Lock()
	if h.p == p {
		h.p = nil
	}
	t := h.turn
	if t != nil && t.proc != p {
		t = nil // the turn belongs to another (newer) process
	}
	if t != nil {
		h.turn = nil
	}
	interrupted := h.interrupting || h.stopped
	h.interrupting = false
	stopped := h.stopped
	h.mu.Unlock()
	if t != nil {
		t.flush()
		tc := model.TurnCompleted{TurnID: t.id, Status: "failed", Error: "agy exited without a result event",
			DurationMs: time.Since(t.start).Milliseconds()}
		if interrupted {
			tc.Status, tc.Error = "interrupted", ""
		} else if err := p.Err(); err != nil {
			tc.Error += ": " + err.Error()
		}
		h.emit(model.KindTurnCompleted, tc)
	}
	if !stopped && t != nil {
		h.setStatus(model.StatusIdle, "")
	}
}

// Interrupt stops the running turn's process; the next turn starts a new
// process on the same conversation.
func (h *handle) Interrupt(ctx context.Context) error {
	h.mu.Lock()
	p := h.p
	if h.turn == nil || p == nil {
		h.mu.Unlock()
		return adapter.ErrNotActive
	}
	h.interrupting = true
	h.mu.Unlock()
	go func() { _ = p.Stop(4 * time.Second) }()
	return nil
}

// Resolve is not supported: headless agy never asks.
func (h *handle) Resolve(ctx context.Context, approvalID, decision string) error {
	return adapter.ErrNotSupported
}

// Stop kills any running process and ends management.
func (h *handle) Stop(ctx context.Context) error {
	h.shutdown(true)
	return nil
}

func (h *handle) shutdown(announce bool) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	p := h.p
	h.mu.Unlock()
	if p != nil {
		_ = p.Stdin.Close() // a clean end for stream-input mode
		_ = p.Stop(4 * time.Second)
	}
	h.a.mu.Lock()
	delete(h.a.hs, h)
	h.a.mu.Unlock()
	if announce {
		h.setStatus(model.StatusStopped, "")
	}
}

// --- stderr ---

// deniedRe matches agy's notice that a tool was soft-denied in headless
// mode (the exact wording is undocumented, so this is deliberately loose).
var deniedRe = regexp.MustCompile(`(?i)(soft[- ]?denied|denied|requires? (approval|confirmation)|not (allowed|permitted)|permission)`)

func (h *handle) stderrLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	low := strings.ToLower(line)
	msg := ""
	switch {
	case strings.Contains(low, "authentication required") || strings.Contains(low, "not signed in"):
		msg = "agy: " + line + " (sign in once by running agy interactively on this host)"
	case deniedRe.MatchString(line):
		msg = "agy: " + line + " (headless agy cannot ask; allow it in agy's settings.json permissions or spawn with sandbox danger-full-access)"
	default:
		return
	}
	h.mu.Lock()
	if h.deniedSeen == nil {
		h.deniedSeen = map[string]bool{}
	}
	dup := h.deniedSeen[line]
	h.deniedSeen[line] = true
	turnID := ""
	if h.turn != nil {
		turnID = h.turn.id
	}
	h.mu.Unlock()
	if !dup || turnID != "" {
		h.emit(model.KindWarning, model.ErrorEvent{TurnID: turnID, Message: truncate(msg, 500)})
	}
}

// lineWriter forwards writes and reports complete lines.
type lineWriter struct {
	w    io.Writer
	line func(string)
	mu   sync.Mutex
	buf  []byte
}

func (l *lineWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, b...)
	var lines []string
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 {
		l.buf = l.buf[:0]
	}
	l.mu.Unlock()
	for _, s := range lines {
		l.line(s)
	}
	return l.w.Write(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
