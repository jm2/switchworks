// SPDX-License-Identifier: GPL-3.0-or-later

// Package acpad adapts Agent Client Protocol agents to the adapter
// interface. Goose (`goose acp`) is the built-in agent; any other ACP agent
// (Crush, opencode, ...) can be configured by name in node.json.
//
// Each session runs its own agent process, so a crash or a stuck turn only
// affects that session, and per-session environment (model provider keys)
// stays separate. Turns are session/prompt calls; tool permission requests
// become Switchworks approvals; Interrupt is session/cancel. After a node
// restart or an agent crash the session is reopened with session/load
// (history replay is discarded: the coordinator already has it) or, if the
// agent only offers that, session/resume.
//
// The fleet tools are attached per session through session/new's
// mcpServers, the same way Codex gets them.
//
// ACP has no mid-turn steering, so Steer is not offered; the node then
// queues messages for the next turn.
package acpad

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/acp"
	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/ulid"
)

// Goose is the built-in ACP agent's harness name.
const Goose = "goose"

// Config configures one ACP agent.
type Config struct {
	Harness string            // harness name, e.g. "goose"
	Bin     string            // agent binary
	Args    []string          // e.g. ["acp"]
	Env     map[string]string // environment overrides (provider keys, GOOSE_PROVIDER, ...)

	// Modes maps a Switchworks approval policy (never, on-request,
	// untrusted) to the agent's session mode id. Unset policies fall back
	// to well-known mode names (see defaultModes).
	Modes map[string]string

	Stderr io.Writer
	Tap    jsonrpc.Tap
	Logf   func(format string, args ...any)
}

// GooseConfig is the built-in configuration for `goose acp`.
func GooseConfig() Config {
	return Config{Harness: Goose, Bin: "goose", Args: []string{"acp"}}
}

// defaultModes lists candidate mode ids per approval policy, in order of
// preference. Goose: auto, smart_approve, approve. Claude-style agents:
// bypassPermissions, default.
var defaultModes = map[string][]string{
	"never":      {"auto", "bypassPermissions", "yolo", "full-access", "full_access"},
	"on-request": {"smart_approve", "default", "ask"},
	"on-failure": {"smart_approve", "default", "ask"},
	"untrusted":  {"approve", "default", "ask"},
}

// cancelGrace is how long an agent may take to honor session/cancel before
// it is killed (and the session reopened).
var cancelGrace = 20 * time.Second

// Adapter runs ACP sessions.
type Adapter struct {
	cfg Config

	mu     sync.Mutex
	hs     map[*handle]bool
	closed bool
}

// New returns an adapter.
func New(cfg Config) *Adapter {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Harness == "" {
		cfg.Harness = Goose
	}
	return &Adapter{cfg: cfg, hs: map[*handle]bool{}}
}

func (a *Adapter) Name() string { return a.cfg.Harness }

// Close stops every agent process.
func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	hs := make([]*handle, 0, len(a.hs))
	for h := range a.hs {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	for _, h := range hs {
		h.shutdown(false) // node shutdown: keep sessions open for resume
	}
	return nil
}

func (a *Adapter) add(h *handle) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return adapter.ErrClosed
	}
	a.hs[h] = true
	return nil
}

func (a *Adapter) remove(h *handle) {
	a.mu.Lock()
	delete(a.hs, h)
	a.mu.Unlock()
}

func (a *Adapter) newHandle(spec adapter.StartSpec, sink adapter.Sink) (*handle, error) {
	if _, err := proc.LookPath(a.cfg.Bin); err != nil {
		return nil, err
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	spec.Cwd = abs // ACP requires an absolute cwd
	h := &handle{a: a, spec: spec, sink: sink, status: model.StatusStarting,
		approvals: map[string]*pendingPerm{}, tools: map[string]*toolState{}}
	if err := a.add(h); err != nil {
		return nil, err
	}
	return h, nil
}

// Start creates a new session.
func (a *Adapter) Start(ctx context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	h, err := a.newHandle(spec, sink)
	if err != nil {
		return nil, err
	}
	if err := h.open(ctx, ""); err != nil {
		a.remove(h)
		return nil, err
	}
	return h, nil
}

// Resume reopens an existing session in a new agent process.
func (a *Adapter) Resume(ctx context.Context, ref model.NativeRef, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if ref.Harness != "" && ref.Harness != a.cfg.Harness {
		return nil, fmt.Errorf("%s adapter cannot resume %s session", a.cfg.Harness, ref.Harness)
	}
	if ref.ID == "" {
		return nil, errors.New("no session id to resume")
	}
	h, err := a.newHandle(spec, sink)
	if err != nil {
		return nil, err
	}
	if err := h.open(ctx, ref.ID); err != nil {
		a.remove(h)
		return nil, err
	}
	return h, nil
}

// --- handle ---

type pendingPerm struct {
	rpcID     json.RawMessage
	turnID    string
	byDecided map[string]string // decision name -> optionId
}

type handle struct {
	a    *Adapter
	spec adapter.StartSpec
	sink adapter.Sink

	mu         sync.Mutex
	c          *acp.Client
	sessionID  string
	modelName  string
	provider   string
	status     string
	turnID     string // active turn
	cancelling bool
	replaying  bool
	closed     bool
	approvals  map[string]*pendingPerm
	tools      map[string]*toolState // by toolCallId, for the active turn
	msg        *textItem             // agent message being streamed
	thought    *textItem             // reasoning being streamed
	diffs      []string              // unified diffs of this turn's edits
	ctxWindow  int64
	lastRep    *model.Usage // last usage the agent reported
	total      model.Usage
	// gooseAcc is goose's accumulated session usage from its custom
	// usage_update, and gooseSent the figure the last usage event was
	// based on (or the first one seen, if none was sent yet).
	gooseAcc  *model.Usage
	gooseSent *model.Usage
	// images: the agent's initialize response advertised
	// promptCapabilities.image (kept across restarts of the agent).
	images    bool
	agentName string
}

type textItem struct {
	id     string
	acpID  string // the agent's messageId, when given
	text   strings.Builder
	turnID string
}

func (h *handle) emit(kind string, p any) { h.sink(adapter.Event{Kind: kind, Payload: p}) }

func (h *handle) Ref() model.NativeRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.NativeRef{Harness: h.a.cfg.Harness, ID: h.sessionID}
}

func (h *handle) Caps() model.Caps {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.Caps{Message: true, Interrupt: true, Stop: true, Approve: true, Images: h.images}
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

func (h *handle) env() map[string]string {
	env := map[string]string{}
	for k, v := range h.a.cfg.Env {
		env[k] = v
	}
	for k, v := range h.spec.Env {
		env[k] = v
	}
	return env
}

func (h *handle) mcpServers() []acp.McpServerStdio {
	m := h.spec.MCP
	if m == nil {
		return []acp.McpServerStdio{}
	}
	args := m.Args
	if args == nil {
		args = []string{}
	}
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]acp.EnvVariable, 0, len(keys))
	for _, k := range keys {
		env = append(env, acp.EnvVariable{Name: k, Value: m.Env[k]})
	}
	cmd := m.Command
	if abs, err := proc.LookPath(cmd); err == nil {
		cmd = abs // ACP wants an absolute path
	}
	return []acp.McpServerStdio{{Name: m.Name, Command: cmd, Args: args, Env: env}}
}

// open starts an agent process and creates (sessionID == "") or reopens a
// session in it, then registers.
func (h *handle) open(ctx context.Context, sessionID string) error {
	if err := proc.CheckWithin(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	c, err := acp.Start(ctx, acp.Options{
		Bin: h.a.cfg.Bin, Args: h.a.cfg.Args, Dir: h.spec.Cwd, Env: h.env(),
		Stderr: h.a.cfg.Stderr, Tap: h.a.cfg.Tap,
	}, h)
	if err != nil {
		return err
	}
	var setup *acp.SessionSetup
	resumed := sessionID != ""
	switch {
	case !resumed:
		setup, err = c.NewSession(ctx, acp.NewSessionRequest{Cwd: h.spec.Cwd, McpServers: h.mcpServers()})
	case c.Info.AgentCapabilities.LoadSession:
		h.mu.Lock()
		h.replaying = true
		h.mu.Unlock()
		setup, err = c.LoadSession(ctx, acp.LoadSessionRequest{SessionID: sessionID, Cwd: h.spec.Cwd, McpServers: h.mcpServers()})
		h.mu.Lock()
		h.replaying = false
		h.mu.Unlock()
	case c.Info.CanResume():
		setup, err = c.ResumeSession(ctx, acp.LoadSessionRequest{SessionID: sessionID, Cwd: h.spec.Cwd, McpServers: h.mcpServers()})
	default:
		err = fmt.Errorf("%s cannot reopen sessions (no session/load or session/resume)", c.Info.AgentName())
	}
	if err != nil {
		_ = c.Kill()
		if resumed {
			return fmt.Errorf("reopen %s session %s: %w", h.a.cfg.Harness, sessionID, err)
		}
		return fmt.Errorf("%s session/new: %w", h.a.cfg.Harness, err)
	}
	if err := h.configure(ctx, c, setup); err != nil {
		_ = c.Kill()
		return err
	}
	h.mu.Lock()
	if h.closed {
		// Stopped (or the node shut down) while the agent was starting:
		// shutdown found no client to stop, so stop this one here.
		h.mu.Unlock()
		_ = c.Kill()
		return adapter.ErrClosed
	}
	h.c = c
	h.sessionID = setup.SessionID
	h.images = c.Info.AgentCapabilities.PromptCapabilities.Image
	h.agentName = c.Info.AgentName()
	h.mu.Unlock()
	h.a.cfg.Logf("%s: session %s %s (pid %d, %s)", h.a.cfg.Harness, setup.SessionID,
		map[bool]string{true: "reopened", false: "started"}[resumed], c.Pid(), c.Info.AgentName())
	h.emit(model.KindSessionRegistered, model.SessionRegistered{
		NativeRef: h.Ref(), Cwd: h.spec.Cwd, Model: h.modelName, Provider: h.provider, Caps: h.Caps(), Resumed: resumed,
	})
	h.setStatus(model.StatusIdle, "")
	go h.watch(c)
	return nil
}

// configure applies provider, model and approval mode. Failures are
// warnings: the session still works with the agent's own defaults.
// configure applies the spec to a new or reopened session. For a session
// an agent spawned (it has a cwd root), failing to apply its approval
// policy is an error: the child must not run with fewer approvals than its
// parent asked for.
func (h *handle) configure(ctx context.Context, c *acp.Client, s *acp.SessionSetup) error {
	var modeErr error
	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		h.a.cfg.Logf("%s: %s", h.a.cfg.Harness, msg)
		h.emit(model.KindWarning, model.ErrorEvent{Message: msg})
	}
	set := func(id, value string) {
		opts, err := c.SetConfigOption(ctx, s.SessionID, id, value)
		if err != nil {
			warn("could not set %s to %q: %v", id, value, err)
			return
		}
		if len(opts) > 0 {
			s.ConfigOptions = opts
		}
	}
	if h.spec.Provider != "" {
		if o := s.Option("provider"); o != nil {
			if o.Current() != h.spec.Provider {
				set("provider", h.spec.Provider)
			}
		} else {
			warn("%s has no provider option; set its provider in its own config or env", h.a.cfg.Harness)
		}
	}
	if h.spec.Model != "" {
		if o := s.Option("model"); o != nil {
			if o.Current() != h.spec.Model {
				set("model", h.spec.Model)
			}
		} else {
			warn("%s has no model option; set its model in its own config or env", h.a.cfg.Harness)
		}
	}
	strict := h.spec.CwdRoot != "" && h.spec.ApprovalPolicy != "" && h.spec.ApprovalPolicy != "never"
	if mode := h.modeFor(s); mode != "" {
		if o := s.Option("mode"); o != nil {
			if o.Current() != mode {
				if opts, err := c.SetConfigOption(ctx, s.SessionID, "mode", mode); err != nil {
					warn("could not set mode to %q: %v", mode, err)
					modeErr = err
				} else if len(opts) > 0 {
					s.ConfigOptions = opts
				}
			}
		} else if s.Modes != nil && s.Modes.CurrentModeID != mode {
			if err := c.SetMode(ctx, s.SessionID, mode); err != nil {
				warn("could not set mode %q: %v", mode, err)
				modeErr = err
			}
		}
	} else if h.spec.ApprovalPolicy != "" {
		if s.Modes != nil || s.Option("mode") != nil {
			warn("no session mode matches approval policy %q; using the agent's default", h.spec.ApprovalPolicy)
		}
		modeErr = fmt.Errorf("no session mode matches approval policy %q", h.spec.ApprovalPolicy)
	}
	if strict && modeErr != nil {
		return fmt.Errorf("%s: cannot apply approval policy %q to a session spawned by an agent: %w", h.a.cfg.Harness, h.spec.ApprovalPolicy, modeErr)
	}
	if o := s.Option("model"); o != nil {
		h.modelName = o.Current()
	} else {
		h.modelName = h.spec.Model
	}
	if o := s.Option("provider"); o != nil {
		h.provider = o.Current()
	} else {
		h.provider = h.spec.Provider
	}
	return nil
}

// modeFor picks the agent's mode for the session's approval policy.
func (h *handle) modeFor(s *acp.SessionSetup) string {
	p := h.spec.ApprovalPolicy
	if p == "" {
		return ""
	}
	has := func(id string) bool {
		if s.HasMode(id) {
			return true
		}
		if o := s.Option("mode"); o != nil {
			return o.Has(id)
		}
		return false
	}
	if m, ok := h.a.cfg.Modes[p]; ok {
		if has(m) {
			return m
		}
		return ""
	}
	for _, m := range defaultModes[p] {
		if has(m) {
			return m
		}
	}
	return ""
}

// watch handles the agent process exiting.
func (h *handle) watch(c *acp.Client) {
	<-c.Done()
	_ = c.Kill() // reap it if stdout closed first
	h.mu.Lock()
	if h.c != c {
		h.mu.Unlock()
		return
	}
	h.c = nil
	closed := h.closed
	sid := h.sessionID
	h.mu.Unlock()
	if closed {
		return
	}
	// A turn in flight fails through its own Prompt call returning.
	h.a.cfg.Logf("%s: agent for session %s exited; reopening", h.a.cfg.Harness, sid)
	h.setStatus(model.StatusError, h.a.cfg.Harness+" exited; reopening the session")
	backoff := time.Second
	for attempt := 0; attempt < 6; attempt++ {
		h.mu.Lock()
		closed = h.closed
		h.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(backoff)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := h.open(ctx, sid)
		cancel()
		if err == nil {
			return
		}
		h.a.cfg.Logf("%s: reopen %s failed: %v", h.a.cfg.Harness, sid, err)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	h.setStatus(model.StatusError, h.a.cfg.Harness+" could not be restarted")
}

func (h *handle) client() (*acp.Client, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, "", adapter.ErrClosed
	}
	if h.c == nil {
		return nil, "", fmt.Errorf("%s is restarting", h.a.cfg.Harness)
	}
	return h.c, h.sessionID, nil
}

// Deliver starts a turn. ACP has no steering.
func (h *handle) Deliver(ctx context.Context, in adapter.Input, mode adapter.DeliveryMode) error {
	if mode == adapter.DeliverSteer {
		return adapter.ErrNotSupported
	}
	c, sid, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	if len(in.Images) > 0 && !h.images {
		name := h.agentName
		h.mu.Unlock()
		if name == "" {
			name = h.a.cfg.Harness
		}
		return fmt.Errorf("%w: %s does not advertise image prompts (promptCapabilities.image) in its ACP initialize response",
			adapter.ErrNoImages, name)
	}
	if h.turnID != "" {
		h.mu.Unlock()
		return adapter.ErrBusy
	}
	turnID := ulid.New()
	h.turnID = turnID
	h.cancelling = false
	h.tools = map[string]*toolState{}
	h.diffs = nil
	h.mu.Unlock()

	userItem := model.Item{TurnID: turnID, ItemID: turnID + "-user", ItemType: "userMessage",
		Raw: model.Marshal(map[string]any{"type": "userMessage", "content": adapter.UserContent(in)})}
	blocks := make([]acp.ContentBlock, 0, len(in.Images))
	for _, im := range in.Images {
		blocks = append(blocks, acp.ContentBlock{Type: "image", Data: base64.StdEncoding.EncodeToString(im.Data), MimeType: im.MIME})
	}
	start := time.Now()
	h.emit(model.KindTurnStarted, model.TurnStarted{TurnID: turnID})
	h.emit(model.KindItemStarted, userItem)
	h.emit(model.KindItemCompleted, userItem)
	h.setStatus(model.StatusActive, "")
	turn, err := c.Prompt(sid, in.Text, blocks...)
	if err != nil {
		h.mu.Lock()
		h.turnID = ""
		h.mu.Unlock()
		h.emit(model.KindTurnCompleted, model.TurnCompleted{TurnID: turnID, Status: "failed", Error: "send prompt: " + err.Error()})
		return fmt.Errorf("%s: %w", h.a.cfg.Harness, err) // the node keeps the message queued
	}
	go h.runTurn(c, turn, turnID, start)
	return nil
}

func (h *handle) runTurn(c *acp.Client, turn *acp.Turn, turnID string, start time.Time) {
	resp, err := turn.Wait(context.Background())
	h.flushText()
	h.finishTools(turnID)
	h.clearApprovals(c, "cleared")

	h.mu.Lock()
	cancelling := h.cancelling
	diffs := h.diffs
	h.mu.Unlock()
	if len(diffs) > 0 {
		h.emit(model.KindTurnDiff, model.TurnDiff{TurnID: turnID, Diff: strings.Join(diffs, "")})
	}
	tc := model.TurnCompleted{TurnID: turnID, DurationMs: time.Since(start).Milliseconds()}
	switch {
	case err != nil && cancelling:
		tc.Status = "interrupted"
	case errors.Is(err, jsonrpc.ErrClosed):
		tc.Status, tc.Error = "failed", h.a.cfg.Harness+" exited during the turn"
	case err != nil:
		tc.Status, tc.Error = "failed", err.Error()
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			tc.Error = rpcErr.Message
			if len(rpcErr.Data) > 0 && string(rpcErr.Data) != "null" {
				tc.Error += ": " + strings.Trim(string(rpcErr.Data), `"`)
			}
		}
	default:
		tc.Status, tc.Error = mapStop(resp.StopReason)
		h.usage(turnID, resp.Usage)
	}
	h.mu.Lock()
	if h.turnID == turnID {
		h.turnID = ""
	}
	closed := h.closed
	h.mu.Unlock()
	alive := true
	select {
	case <-c.Done():
		alive = false // watch reports the exit and reopens the session
	default:
	}
	h.emit(model.KindTurnCompleted, tc)
	if !closed && alive {
		h.setStatus(model.StatusIdle, "")
	}
}

func mapStop(r string) (status, errText string) {
	switch r {
	case acp.StopEndTurn, "":
		return "completed", ""
	case acp.StopCancelled:
		return "interrupted", ""
	case acp.StopMaxTokens:
		return "failed", "the model hit its output token limit"
	case acp.StopMaxTurnRequests:
		return "failed", "the agent hit its limit of model requests for one turn"
	case acp.StopRefusal:
		return "failed", "the model refused to continue"
	}
	return "failed", "stop reason " + r
}

// usage turns the agent's report into a Switchworks usage event. The spec
// says the figures are cumulative for the session; some agents report
// something else. Goose 1.55 reports its last model request (the context
// in use), not the turn's sum; its accumulated session usage comes in its
// _goose/unstable/session/update notification (OnGooseUpdate), which is
// used whenever goose sends it. Without it, goose's report is taken as is
// and a multi-request turn counts only its final request. For other
// agents a report is taken as cumulative (the previous one subtracted)
// only when no field shrank; otherwise it is this turn's own. Total counts
// from this agent process only (it restarts from zero when the session is
// reopened); the coordinator keeps the session's running sum.
func (h *handle) usage(turnID string, u *acp.Usage) {
	var rep model.Usage
	if u != nil {
		rep = model.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
		if u.CachedReadTokens != nil {
			rep.CachedInputTokens = *u.CachedReadTokens
		}
		if u.ThoughtTokens != nil {
			rep.ReasoningTokens = *u.ThoughtTokens
		}
		if rep.TotalTokens == 0 {
			rep.TotalTokens = rep.InputTokens + rep.OutputTokens
		}
	}
	h.mu.Lock()
	// goose may leave the prompt response's usage out; its accumulated
	// figures still count.
	if acc := h.gooseAcc; acc != nil {
		// goose's accumulated usage: the whole session's, all requests.
		// Total continues across reopens, which the coordinator expects
		// of a Total that only grows.
		last := rep
		if sent := h.gooseSent; sent != nil && covers(*acc, *sent) {
			last = model.Usage{InputTokens: acc.InputTokens - sent.InputTokens,
				OutputTokens: acc.OutputTokens - sent.OutputTokens, TotalTokens: acc.TotalTokens - sent.TotalTokens}
		}
		cur := *acc
		h.gooseSent = &cur
		if u != nil {
			h.lastRep = &rep
		}
		h.total = cur
		ev := model.UsageEvent{TurnID: turnID, Model: h.modelName, Provider: h.provider, Last: last, Total: cur, ContextWindow: h.ctxWindow}
		h.mu.Unlock()
		h.emit(model.KindUsage, ev)
		return
	}
	if u == nil {
		h.mu.Unlock()
		return
	}
	last := rep
	if p := h.lastRep; p != nil && !reportsLastRequest(h.agentName) {
		// A field the agent left out this time is unknown, not zero: as a
		// cumulative report it keeps its last value, so leaving it out
		// cannot make the report look like a reset.
		cum := rep
		if u.CachedReadTokens == nil {
			cum.CachedInputTokens = p.CachedInputTokens
		}
		if u.ThoughtTokens == nil {
			cum.ReasoningTokens = p.ReasoningTokens
		}
		if cum.TotalTokens > p.TotalTokens && covers(cum, *p) {
			last = model.Usage{
				InputTokens: cum.InputTokens - p.InputTokens, CachedInputTokens: cum.CachedInputTokens - p.CachedInputTokens,
				OutputTokens: cum.OutputTokens - p.OutputTokens, ReasoningTokens: cum.ReasoningTokens - p.ReasoningTokens,
				TotalTokens: cum.TotalTokens - p.TotalTokens,
			}
			rep = cum
		}
	}
	h.lastRep = &rep
	restarted := h.total == (model.Usage{}) // this handle's first report
	h.total.InputTokens += last.InputTokens
	h.total.CachedInputTokens += last.CachedInputTokens
	h.total.OutputTokens += last.OutputTokens
	h.total.ReasoningTokens += last.ReasoningTokens
	h.total.TotalTokens += last.TotalTokens
	ev := model.UsageEvent{TurnID: turnID, Model: h.modelName, Provider: h.provider, Last: last, Total: h.total,
		ContextWindow: h.ctxWindow, Restarted: restarted}
	h.mu.Unlock()
	h.emit(model.KindUsage, ev)
}

// OnGooseUpdate reads goose's accumulated session usage, sent after
// session/new and session/load, after each model request and at the end of
// each turn.
func (h *handle) OnGooseUpdate(n acp.GooseSessionNotification) {
	var u acp.GooseUsageUpdate
	if json.Unmarshal(n.Update, &u) != nil || u.SessionUpdate != acp.GooseUpdateUsage {
		return
	}
	acc := model.Usage{InputTokens: u.AccumulatedInputTokens, OutputTokens: u.AccumulatedOutputTokens,
		TotalTokens: u.AccumulatedInputTokens + u.AccumulatedOutputTokens}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n.SessionID != h.sessionID && h.sessionID != "" {
		return
	}
	h.gooseAcc = &acc
	if h.gooseSent == nil {
		base := acc
		h.gooseSent = &base
	}
	if u.ContextLimit > 0 {
		h.ctxWindow = u.ContextLimit
	}
}

// reportsLastRequest: the agent's PromptResponse usage is its last model
// request, never cumulative (goose: build_prompt_usage copies the session's
// current-request usage), so it must not be differenced.
func reportsLastRequest(agentName string) bool {
	name, _, _ := strings.Cut(strings.ToLower(agentName), " ")
	return name == "goose"
}

// covers reports whether every field of a is at least b's: only then can a
// be a later cumulative report than b.
func covers(a, b model.Usage) bool {
	return a.InputTokens >= b.InputTokens && a.CachedInputTokens >= b.CachedInputTokens &&
		a.OutputTokens >= b.OutputTokens && a.ReasoningTokens >= b.ReasoningTokens &&
		a.TotalTokens >= b.TotalTokens
}

// Interrupt cancels the running turn. Pending permission prompts are
// answered "cancelled", as the protocol requires. An agent that does not
// end the turn within cancelGrace is killed and the session reopened.
func (h *handle) Interrupt(ctx context.Context) error {
	c, sid, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	turnID := h.turnID
	if turnID != "" {
		h.cancelling = true
	}
	h.mu.Unlock()
	if turnID == "" {
		return adapter.ErrNotActive
	}
	h.clearApprovals(c, "cancel")
	if err := c.Cancel(sid); err != nil {
		return err
	}
	grace := cancelGrace
	go func() {
		time.Sleep(grace)
		h.mu.Lock()
		stuck := h.turnID == turnID && h.c == c
		h.mu.Unlock()
		if stuck {
			h.a.cfg.Logf("%s: session %s ignored session/cancel; restarting the agent", h.a.cfg.Harness, sid)
			_ = c.Kill()
		}
	}()
	return nil
}

// Stop ends management: cancels any turn, closes the session if the agent
// supports it, and stops the agent process. The agent keeps its history.
func (h *handle) Stop(ctx context.Context) error {
	h.shutdown(true)
	return nil
}

// shutdown stops the agent. stop (a user's Stop) also closes the session
// and reports it stopped; a node shutdown leaves it to be reopened.
func (h *handle) shutdown(stop bool) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	c, sid, active := h.c, h.sessionID, h.turnID != ""
	h.cancelling = true
	h.mu.Unlock()
	if c != nil {
		h.clearApprovals(c, "cancel")
		if active {
			_ = c.Cancel(sid)
		}
		if stop && c.Info.CanClose() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.CloseSession(ctx, sid)
			cancel()
		}
		_ = c.Close()
	}
	h.a.remove(h)
	if stop {
		h.setStatus(model.StatusStopped, "")
	}
}

// --- approvals ---

// Decision names map onto ACP permission option kinds, keeping Codex's
// vocabulary where it fits so the UI and CLI need no special cases.
var decisionForKind = map[string]string{
	acp.AllowOnce:    "accept",
	acp.AllowAlways:  "acceptForSession",
	acp.RejectOnce:   "decline",
	acp.RejectAlways: "declineAlways",
}

// DecisionCancel declines and cancels the whole turn.
const DecisionCancel = "cancel"

func (h *handle) OnPermission(id json.RawMessage, req acp.RequestPermissionRequest) {
	h.mu.Lock()
	c := h.c
	turnID := h.turnID
	ts := h.toolLocked(req.ToolCall.ToolCallID, turnID)
	ts.merge(req.ToolCall.ToolCallFields, req.ToolCall.ToolContent())
	cl := ts.classify()
	startItem := !ts.started
	ts.started = true
	item := ts.item()
	h.mu.Unlock()
	if c == nil {
		return
	}
	if startItem {
		h.emit(model.KindItemStarted, item)
	}
	// The fleet tools are approved automatically, as for Codex: Switchworks
	// applies its own guards to fleet operations, and an agent waiting on
	// a person to approve fleet_reply would stall agent-to-agent traffic.
	if m := h.spec.MCP; m != nil && cl.itemType == "mcpToolCall" && cl.server == m.Name {
		for _, kind := range []string{acp.AllowOnce, acp.AllowAlways} {
			for _, o := range req.Options {
				if o.Kind == kind {
					_ = c.Permit(id, acp.PermissionOutcome{Outcome: "selected", OptionID: o.OptionID})
					return
				}
			}
		}
	}
	aid := ulid.New()
	p := &pendingPerm{rpcID: append(json.RawMessage(nil), id...), turnID: turnID, byDecided: map[string]string{}}
	var decisions []string
	for _, o := range req.Options {
		d := decisionForKind[o.Kind]
		if d == "" || p.byDecided[d] != "" {
			d = o.OptionID // unknown kind or duplicate: offer the raw option
		}
		p.byDecided[d] = o.OptionID
		decisions = append(decisions, d)
	}
	decisions = append(decisions, DecisionCancel)
	ar := model.ApprovalRequested{ApprovalID: aid, TurnID: turnID, ItemID: ts.itemID, Decisions: decisions, Cwd: h.spec.Cwd}
	switch cl.itemType {
	case "commandExecution":
		ar.Kind, ar.Command = "command", cl.command
	case "fileChange":
		ar.Kind, ar.Command = "file_change", ts.title
		ar.Reason = strings.Join(cl.paths, ", ")
	default:
		ar.Kind, ar.Command = "tool", ts.title
	}
	if ar.Command == "" {
		ar.Command = ts.title
	}
	h.mu.Lock()
	h.approvals[aid] = p
	h.mu.Unlock()
	h.emit(model.KindApprovalRequested, ar)
	h.setStatus(model.StatusWaitingApproval, "")
}

// Resolve answers a pending permission request.
func (h *handle) Resolve(ctx context.Context, approvalID, decision string) error {
	c, sid, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	if !ok {
		h.mu.Unlock()
		return adapter.ErrUnknown
	}
	optionID := p.byDecided[decision]
	if decision != DecisionCancel && optionID == "" {
		offered := make([]string, 0, len(p.byDecided)+1)
		for d := range p.byDecided {
			offered = append(offered, d)
		}
		sort.Strings(offered)
		h.mu.Unlock()
		return fmt.Errorf("decision %q not offered for this prompt (offered: %v, cancel)", decision, offered)
	}
	delete(h.approvals, approvalID)
	remaining := len(h.approvals)
	if decision == DecisionCancel {
		h.cancelling = true
	}
	h.mu.Unlock()

	if decision == DecisionCancel {
		err = c.Permit(p.rpcID, acp.PermissionOutcome{Outcome: "cancelled"})
		h.clearApprovals(c, "cancel")
		_ = c.Cancel(sid)
	} else {
		err = c.Permit(p.rpcID, acp.PermissionOutcome{Outcome: "selected", OptionID: optionID})
	}
	if err != nil {
		return err
	}
	h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: approvalID, Decision: decision})
	if remaining == 0 && decision != DecisionCancel {
		h.mu.Lock()
		active := h.turnID != ""
		h.mu.Unlock()
		if active {
			h.setStatus(model.StatusActive, "")
		}
	}
	return nil
}

// clearApprovals answers every pending permission request "cancelled".
func (h *handle) clearApprovals(c *acp.Client, decision string) {
	h.mu.Lock()
	ps := h.approvals
	h.approvals = map[string]*pendingPerm{}
	h.mu.Unlock()
	ids := make([]string, 0, len(ps))
	for aid := range ps {
		ids = append(ids, aid)
	}
	sort.Strings(ids)
	for _, aid := range ids {
		if c != nil {
			_ = c.Permit(ps[aid].rpcID, acp.PermissionOutcome{Outcome: "cancelled"})
		}
		h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: aid, Decision: decision})
	}
}

// --- session/update ---

func (h *handle) OnUpdate(n acp.SessionNotification) {
	var u acp.Update
	if err := json.Unmarshal(n.Update, &u); err != nil {
		return
	}
	h.mu.Lock()
	if h.replaying {
		h.mu.Unlock()
		return
	}
	turnID := h.turnID
	h.mu.Unlock()

	switch u.SessionUpdate {
	case acp.UpdateAgentMessageChunk:
		h.chunk(&h.msg, "agentMessage", turnID, u.MessageID, u.Text(), true)
	case acp.UpdateAgentThoughtChunk:
		h.chunk(&h.thought, "reasoning", turnID, u.MessageID, u.Text(), false)
	case acp.UpdateToolCall, acp.UpdateToolCallUpdate:
		h.flushText()
		h.tool(turnID, u)
	case acp.UpdatePlan:
		h.flushText()
		entries := u.Entries
		if entries == nil {
			entries = []acp.PlanEntry{}
		}
		h.emit(model.KindItemCompleted, model.Item{TurnID: turnID, ItemID: ulid.New(), ItemType: "plan",
			Raw: model.Marshal(map[string]any{"type": "plan", "entries": entries})})
	case acp.UpdateUsage:
		if u.Size != nil {
			h.mu.Lock()
			h.ctxWindow = *u.Size
			h.mu.Unlock()
		}
	case acp.UpdateNotice:
		msg := u.Title
		text := ""
		if msg != nil {
			text = *msg
		}
		if u.Description != "" {
			text += ": " + u.Description
		}
		kind := model.KindWarning
		if u.Severity == "error" {
			kind = model.KindError
		}
		h.emit(kind, model.ErrorEvent{TurnID: turnID, Message: text})
	case acp.UpdateCompaction:
		h.emit(model.KindWarning, model.ErrorEvent{TurnID: turnID, Message: h.a.cfg.Harness + " compacted the conversation context"})
	}
}

// chunk appends streamed text to the current message or thought item,
// starting a new item when the agent's message id changes.
func (h *handle) chunk(slot **textItem, itemType, turnID, acpID, text string, stream bool) {
	if text == "" {
		return
	}
	h.mu.Lock()
	cur := *slot
	var done *textItem
	if cur != nil && acpID != "" && cur.acpID != "" && cur.acpID != acpID {
		done, cur = cur, nil
	}
	started := false
	if cur == nil {
		cur = &textItem{id: ulid.New(), acpID: acpID, turnID: turnID}
		*slot = cur
		started = true
	}
	cur.text.WriteString(text)
	h.mu.Unlock()
	if done != nil {
		h.completeText(done, itemType)
	}
	if started {
		h.emit(model.KindItemStarted, model.Item{TurnID: turnID, ItemID: cur.id, ItemType: itemType})
	}
	if stream {
		h.emit(model.KindItemDelta, model.ItemDelta{TurnID: turnID, ItemID: cur.id, Text: text})
	}
}

func (h *handle) completeText(t *textItem, itemType string) {
	text := t.text.String()
	h.emit(model.KindItemCompleted, model.Item{TurnID: t.turnID, ItemID: t.id, ItemType: itemType, Text: text,
		Raw: model.Marshal(map[string]any{"type": itemType, "text": text})})
}

// flushText completes the streamed message and thought items.
func (h *handle) flushText() {
	h.mu.Lock()
	m, t := h.msg, h.thought
	h.msg, h.thought = nil, nil
	h.mu.Unlock()
	if t != nil {
		h.completeText(t, "reasoning")
	}
	if m != nil {
		h.completeText(m, "agentMessage")
	}
}

func (h *handle) toolLocked(id, turnID string) *toolState {
	ts := h.tools[id]
	if ts == nil {
		ts = &toolState{id: id, itemID: "tool-" + id, turnID: turnID}
		if id == "" {
			ts.itemID = ulid.New()
		}
		h.tools[id] = ts
	}
	return ts
}

func (h *handle) tool(turnID string, u acp.Update) {
	h.mu.Lock()
	ts := h.toolLocked(u.ToolCallID, turnID)
	var content []acp.ToolCallContent
	if len(u.Content) > 0 {
		content = u.ToolContent()
	}
	ts.merge(u.ToolCallFields, content)
	ts.mergeMeta(u.Meta)
	emitStart := !ts.started
	ts.started = true
	done := ts.terminal() && !ts.completed
	if done {
		ts.completed = true
	}
	item := ts.item()
	var diff string
	if done {
		diff = ts.diffText()
		if diff != "" {
			h.diffs = append(h.diffs, diff)
		}
	}
	h.mu.Unlock()
	if emitStart {
		h.emit(model.KindItemStarted, item)
	}
	if done {
		h.emit(model.KindItemCompleted, item)
	}
}

// finishTools completes tool items the agent left open when the turn ended.
func (h *handle) finishTools(turnID string) {
	h.mu.Lock()
	var items []model.Item
	for _, ts := range h.tools {
		if ts.started && !ts.completed {
			ts.completed = true
			if !ts.terminal() {
				ts.status = acp.ToolFailed
				ts.unfinished = true
			}
			items = append(items, ts.item())
		}
	}
	h.tools = map[string]*toolState{}
	h.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ItemID < items[j].ItemID })
	for _, it := range items {
		h.emit(model.KindItemCompleted, it)
	}
}
