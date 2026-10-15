// SPDX-License-Identifier: GPL-3.0-or-later

// Package codexad adapts `codex app-server` to the adapter interface.
//
// One app-server process per node hosts every managed thread; notifications
// are routed to sessions by threadId. If app-server exits, the adapter starts
// a new one and resumes every live thread with thread/resume.
package codexad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/codex"
	"github.com/jm2/switchyard/internal/jsonrpc"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/proc"
	"github.com/jm2/switchyard/internal/ulid"
)

// Harness is this adapter's name.
const Harness = "codex"

// Config configures the adapter.
type Config struct {
	Bin    string            // codex binary; default "codex"
	Args   []string          // default ["app-server"]
	Env    map[string]string // e.g. CODEX_HOME
	Dir    string            // app-server working directory
	Stderr io.Writer         // app-server logs
	Tap    jsonrpc.Tap       // raw protocol capture
	Logf   func(format string, args ...any)
}

// Adapter manages one app-server process and its threads.
type Adapter struct {
	cfg Config

	mu      sync.Mutex
	client  *codex.Client
	threads map[string]*handle
	early   map[string][]note // notifications for threads not yet registered
	closed  bool
	restart sync.Mutex
}

type note struct {
	method string
	params json.RawMessage
}

// New returns an adapter; app-server starts on first use.
func New(cfg Config) *Adapter {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Adapter{cfg: cfg, threads: map[string]*handle{}, early: map[string][]note{}}
}

func (a *Adapter) Name() string { return Harness }

// Info returns the initialize response of the running app-server, if any.
func (a *Adapter) Info() (codex.InitializeResponse, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		return codex.InitializeResponse{}, false
	}
	return a.client.Info, true
}

func (a *Adapter) getClient(ctx context.Context) (*codex.Client, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, adapter.ErrClosed
	}
	if c := a.client; c != nil {
		select {
		case <-c.Done():
		default:
			a.mu.Unlock()
			return c, nil
		}
	}
	a.mu.Unlock()

	a.restart.Lock()
	defer a.restart.Unlock()
	a.mu.Lock()
	if c := a.client; c != nil {
		select {
		case <-c.Done():
		default:
			a.mu.Unlock()
			return c, nil
		}
	}
	a.mu.Unlock()
	c, err := codex.Start(ctx, codex.Options{
		Bin: a.cfg.Bin, Args: a.cfg.Args, Dir: a.cfg.Dir, Env: a.cfg.Env, Stderr: a.cfg.Stderr, Tap: a.cfg.Tap,
	}, a)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.client = c
	a.mu.Unlock()
	a.cfg.Logf("codex app-server started (pid %d, codex home %s)", c.Pid(), c.Info.CodexHome)
	go a.watch(c)
	return c, nil
}

// watch restarts app-server after an unexpected exit and resumes threads.
func (a *Adapter) watch(c *codex.Client) {
	<-c.Done()
	a.mu.Lock()
	if a.closed || a.client != c {
		a.mu.Unlock()
		return
	}
	a.client = nil
	live := make([]*handle, 0, len(a.threads))
	for _, h := range a.threads {
		live = append(live, h)
	}
	a.mu.Unlock()
	a.cfg.Logf("codex app-server exited; restarting and resuming %d thread(s)", len(live))
	for _, h := range live {
		h.lostClient()
	}
	backoff := time.Second
	for attempt := 0; attempt < 8; attempt++ {
		a.mu.Lock()
		closed := a.closed
		a.mu.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		nc, err := a.getClient(ctx)
		if err == nil {
			for _, h := range live {
				if err := h.resumeOn(ctx, nc); err != nil {
					h.setStatus(model.StatusError, "resume after app-server restart failed: "+err.Error())
				}
			}
			cancel()
			return
		}
		cancel()
		a.cfg.Logf("codex app-server restart failed: %v", err)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	for _, h := range live {
		h.setStatus(model.StatusError, "codex app-server could not be restarted")
	}
}

// Close stops app-server.
func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	c := a.client
	a.client = nil
	a.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// OnNotification routes app-server notifications to sessions.
func (a *Adapter) OnNotification(method string, params json.RawMessage) {
	tid := codex.ThreadIDOf(params)
	if tid == "" {
		return
	}
	a.mu.Lock()
	h := a.threads[tid]
	if h == nil {
		// The thread/start response may still be in flight; keep a few
		// notifications so they can be replayed once the handle exists.
		if q := a.early[tid]; len(q) < 128 && len(a.early) < 64 {
			a.early[tid] = append(q, note{method, params})
		}
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	h.onNote(method, params)
}

// OnServerRequest handles approvals and rejects everything else.
func (a *Adapter) OnServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	a.mu.Lock()
	c := a.client
	h := a.threads[codex.ThreadIDOf(params)]
	a.mu.Unlock()
	if c == nil {
		return
	}
	if h != nil && (method == codex.RequestCommandApproval || method == codex.RequestFileChangeApproval) {
		h.onApproval(c, id, method, params)
		return
	}
	a.cfg.Logf("codex: rejecting unsupported server request %s", method)
	_ = c.ReplyError(id, jsonrpc.CodeMethodNotFound, "switchyard does not support "+method+" yet")
}

func (a *Adapter) register(h *handle) []note {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threads[h.threadID] = h
	q := a.early[h.threadID]
	delete(a.early, h.threadID)
	return q
}

func (a *Adapter) unregister(h *handle) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.threads[h.threadID] == h {
		delete(a.threads, h.threadID)
	}
}

// Start creates a new thread.
func (a *Adapter) Start(ctx context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if err := proc.CheckWithin(spec.CwdRoot, spec.Cwd); err != nil {
		return nil, err
	}
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.ThreadStart(ctx, codex.ThreadStartParams{
		Cwd:            spec.Cwd,
		Model:          spec.Model,
		ModelProvider:  spec.Provider,
		ApprovalPolicy: spec.ApprovalPolicy,
		Sandbox:        spec.Sandbox,
		ServiceName:    "switchyard",
		Config:         mcpConfig(spec.MCP),
	})
	if err != nil {
		return nil, fmt.Errorf("thread/start: %w", err)
	}
	h := newHandle(a, c, resp.Thread.ID, spec, sink)
	h.attached(c, resp, false)
	if spec.Name != "" {
		if err := c.ThreadSetName(ctx, h.threadID, spec.Name); err != nil {
			a.cfg.Logf("codex: thread/name/set: %v", err)
		}
	}
	return h, nil
}

// Resume reattaches to an existing thread.
func (a *Adapter) Resume(ctx context.Context, ref model.NativeRef, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if ref.Harness != "" && ref.Harness != Harness {
		return nil, fmt.Errorf("codex adapter cannot resume %s session", ref.Harness)
	}
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	h := newHandle(a, c, ref.ID, spec, sink)
	if err := h.resumeOn(ctx, c); err != nil {
		return nil, err
	}
	return h, nil
}

type pendingApproval struct {
	rpcID     json.RawMessage
	turnID    string
	decisions []string // offered by Codex; empty = default set
}

type handle struct {
	a        *Adapter
	threadID string
	spec     adapter.StartSpec
	sink     adapter.Sink

	mu         sync.Mutex
	c          *codex.Client
	status     string
	activeTurn string
	doneTurns  []string // recently completed turn ids (bounded)
	approvals  map[string]pendingApproval
	model      string
	provider   string
	home       string
	closed     bool
}

func newHandle(a *Adapter, c *codex.Client, threadID string, spec adapter.StartSpec, sink adapter.Sink) *handle {
	return &handle{
		a: a, c: c, threadID: threadID, spec: spec, sink: sink,
		status: model.StatusStarting, approvals: map[string]pendingApproval{},
		home: c.Info.CodexHome,
	}
}

func (h *handle) emit(kind string, payload any) {
	h.sink(adapter.Event{Kind: kind, Payload: payload})
}

func (h *handle) caps() model.Caps {
	return model.Caps{Message: true, Steer: true, Interrupt: true, Stop: true, Approve: true}
}

// attached finishes registration after thread/start or thread/resume.
func (h *handle) attached(c *codex.Client, resp *codex.ThreadStartResponse, resumed bool) {
	h.mu.Lock()
	h.c = c
	h.model = resp.Model
	h.provider = resp.ModelProvider
	h.home = c.Info.CodexHome
	h.activeTurn = ""
	h.mu.Unlock()
	early := h.a.register(h)
	h.emit(model.KindSessionRegistered, model.SessionRegistered{
		NativeRef: h.Ref(), Cwd: resp.Cwd, Model: resp.Model, Provider: resp.ModelProvider,
		Caps: h.caps(), Resumed: resumed,
	})
	st := model.StatusIdle
	if resp.Thread.Status != nil {
		st = mapThreadStatus(*resp.Thread.Status)
	}
	h.setStatus(st, "")
	for _, n := range early {
		h.onNote(n.method, n.params)
	}
}

func (h *handle) resumeOn(ctx context.Context, c *codex.Client) error {
	if err := proc.CheckWithin(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	resp, err := c.ThreadResume(ctx, codex.ThreadResumeParams{
		ThreadID:       h.threadID,
		ApprovalPolicy: h.spec.ApprovalPolicy,
		Sandbox:        h.spec.Sandbox,
		ExcludeTurns:   true,
		Config:         mcpConfig(h.spec.MCP),
	})
	if err != nil {
		return fmt.Errorf("thread/resume %s: %w", h.threadID, err)
	}
	h.attached(c, resp, true)
	return nil
}

func (h *handle) lostClient() {
	h.mu.Lock()
	h.c = nil
	h.activeTurn = ""
	for id := range h.approvals {
		delete(h.approvals, id)
	}
	h.mu.Unlock()
	h.setStatus(model.StatusError, "codex app-server exited; restarting")
}

func (h *handle) client() (*codex.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, adapter.ErrClosed
	}
	if h.c == nil {
		return nil, errors.New("codex app-server is restarting")
	}
	return h.c, nil
}

func (h *handle) Ref() model.NativeRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.NativeRef{Harness: Harness, ID: h.threadID, Home: h.home}
}

func (h *handle) Caps() model.Caps { return h.caps() }

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

func mapThreadStatus(s codex.ThreadStatus) string {
	switch s.Type {
	case "idle":
		return model.StatusIdle
	case "active":
		for _, f := range s.ActiveFlags {
			if f == "waitingOnApproval" {
				return model.StatusWaitingApproval
			}
		}
		return model.StatusActive
	case "systemError":
		return model.StatusError
	case "notLoaded":
		return model.StatusGone
	}
	return model.StatusIdle
}

func (h *handle) onNote(method string, params json.RawMessage) {
	switch method {
	case codex.NoteThreadStatusChanged:
		var n codex.ThreadStatusChangedNotification
		if json.Unmarshal(params, &n) == nil {
			h.setStatus(mapThreadStatus(n.Status), "")
		}
	case codex.NoteThreadClosed:
		h.setStatus(model.StatusGone, "thread closed by codex")
	case codex.NoteTurnStarted:
		var n codex.TurnNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			h.activeTurn = n.Turn.ID
			h.mu.Unlock()
			h.emit(model.KindTurnStarted, model.TurnStarted{TurnID: n.Turn.ID})
		}
	case codex.NoteTurnCompleted:
		var n codex.TurnNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			if h.activeTurn == n.Turn.ID {
				h.activeTurn = ""
			}
			h.doneTurns = append(h.doneTurns, n.Turn.ID)
			if len(h.doneTurns) > 16 {
				h.doneTurns = h.doneTurns[len(h.doneTurns)-16:]
			}
			h.mu.Unlock()
			tc := model.TurnCompleted{TurnID: n.Turn.ID, Status: n.Turn.Status}
			if n.Turn.Error != nil {
				tc.Error = n.Turn.Error.Message
			}
			if n.Turn.DurationMs != nil {
				tc.DurationMs = *n.Turn.DurationMs
			}
			h.emit(model.KindTurnCompleted, tc)
		}
	case codex.NoteItemStarted, codex.NoteItemCompleted:
		var n codex.ItemNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		var hdr codex.ItemHeader
		_ = json.Unmarshal(n.Item, &hdr)
		it := model.Item{TurnID: n.TurnID, ItemID: hdr.ID, ItemType: hdr.Type, Raw: n.Item}
		if hdr.Type == "agentMessage" {
			var am codex.AgentMessageItem
			if json.Unmarshal(n.Item, &am) == nil {
				it.Text = am.Text
			}
		}
		kind := model.KindItemStarted
		if method == codex.NoteItemCompleted {
			kind = model.KindItemCompleted
		}
		h.emit(kind, it)
	case codex.NoteAgentMessageDelta:
		var n codex.AgentMessageDeltaNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindItemDelta, model.ItemDelta{TurnID: n.TurnID, ItemID: n.ItemID, Text: n.Delta})
		}
	case codex.NoteTurnDiffUpdated:
		var n codex.TurnDiffUpdatedNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindTurnDiff, model.TurnDiff{TurnID: n.TurnID, Diff: n.Diff})
		}
	case codex.NoteTokenUsageUpdated:
		var n codex.ThreadTokenUsageUpdatedNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			m, p := h.model, h.provider
			h.mu.Unlock()
			ev := model.UsageEvent{TurnID: n.TurnID, Model: m, Provider: p,
				Last: usage(n.TokenUsage.Last), Total: usage(n.TokenUsage.Total)}
			if n.TokenUsage.ModelContextWindow != nil {
				ev.ContextWindow = *n.TokenUsage.ModelContextWindow
			}
			h.emit(model.KindUsage, ev)
		}
	case codex.NoteError:
		var n codex.ErrorNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindError, model.ErrorEvent{TurnID: n.TurnID, Message: n.Error.Message, WillRetry: n.WillRetry})
		}
	case codex.NoteRequestResolved:
		var n codex.ServerRequestResolvedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		h.mu.Lock()
		var cleared []string
		for aid, p := range h.approvals {
			if string(p.rpcID) == string(n.RequestID) {
				cleared = append(cleared, aid)
				delete(h.approvals, aid)
			}
		}
		h.mu.Unlock()
		for _, aid := range cleared {
			h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: aid, Decision: "cleared"})
		}
	}
}

func usage(b codex.TokenUsageBreakdown) model.Usage {
	return model.Usage{
		InputTokens: b.InputTokens, CachedInputTokens: b.CachedInputTokens, OutputTokens: b.OutputTokens,
		ReasoningTokens: b.ReasoningOutputTokens, TotalTokens: b.TotalTokens,
	}
}

// mcpConfig renders a per-thread config override that attaches an MCP
// server to this thread only. Its tools are auto-approved
// (default_tools_approval_mode = "approve"): Codex otherwise asks before
// every MCP call, which fails outright under approval policy "never".
// Switchyard applies its own guards to fleet operations.
func mcpConfig(m *adapter.MCPServer) map[string]any {
	if m == nil {
		return nil
	}
	timeout := m.ToolTimeout
	if timeout <= 0 {
		timeout = 60
	}
	args := m.Args
	if args == nil {
		args = []string{}
	}
	return map[string]any{"mcp_servers": map[string]any{m.Name: map[string]any{
		"command":                     m.Command,
		"args":                        args,
		"env":                         m.Env,
		"startup_timeout_sec":         30,
		"tool_timeout_sec":            timeout,
		"default_tools_approval_mode": "approve",
	}}}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *handle) onApproval(c *codex.Client, id json.RawMessage, method string, params json.RawMessage) {
	aid := ulid.New()
	req := model.ApprovalRequested{ApprovalID: aid}
	switch method {
	case codex.RequestCommandApproval:
		var p codex.CommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchyard: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID = "command", p.TurnID, p.ItemID
		req.Command, req.Cwd, req.Reason = deref(p.Command), deref(p.Cwd), deref(p.Reason)
		req.Decisions = codex.DecisionNames(p.AvailableDecisions)
	case codex.RequestFileChangeApproval:
		var p codex.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchyard: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID, req.Reason = "file_change", p.TurnID, p.ItemID, deref(p.Reason)
	}
	h.mu.Lock()
	h.approvals[aid] = pendingApproval{rpcID: append(json.RawMessage(nil), id...), turnID: req.TurnID, decisions: req.Decisions}
	h.mu.Unlock()
	h.emit(model.KindApprovalRequested, req)
	h.setStatus(model.StatusWaitingApproval, "")
}

// Resolve answers a pending approval.
func (h *handle) Resolve(ctx context.Context, approvalID, decision string) error {
	switch decision {
	case codex.DecisionAccept, codex.DecisionAcceptForSession, codex.DecisionDecline, codex.DecisionCancel:
	default:
		return fmt.Errorf("unknown decision %q (accept, acceptForSession, decline, cancel)", decision)
	}
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	if ok && len(p.decisions) > 0 && !containsStr(p.decisions, decision) {
		h.mu.Unlock()
		return fmt.Errorf("decision %q not offered for this prompt (offered: %v)", decision, p.decisions)
	}
	if ok {
		delete(h.approvals, approvalID)
	}
	h.mu.Unlock()
	if !ok {
		return adapter.ErrUnknown
	}
	if err := c.Reply(p.rpcID, codex.ApprovalResponse{Decision: decision}); err != nil {
		return err
	}
	h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: approvalID, Decision: decision})
	return nil
}

// Deliver starts a turn (queue) or steers the active one.
func (h *handle) Deliver(ctx context.Context, in adapter.Input, mode adapter.DeliveryMode) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	active := h.activeTurn
	status := h.status
	h.mu.Unlock()
	switch mode {
	case adapter.DeliverSteer:
		if active == "" {
			return adapter.ErrNotActive
		}
		_, err := c.TurnSteer(ctx, codex.TurnSteerParams{
			ThreadID: h.threadID, ExpectedTurnID: active, Input: codex.TextInput(in.Text),
			ClientUserMessageID: in.ClientMessageID,
		})
		return err
	default:
		if active != "" || status == model.StatusActive || status == model.StatusWaitingApproval {
			return adapter.ErrBusy
		}
		resp, err := c.TurnStart(ctx, codex.TurnStartParams{
			ThreadID: h.threadID, Input: codex.TextInput(in.Text), ClientUserMessageID: in.ClientMessageID,
		})
		if err != nil {
			return err
		}
		// Notifications for this turn may already have been processed (a
		// fast turn can complete before the turn/start response is read
		// here), so only adopt it if it has not finished.
		h.mu.Lock()
		if resp.Turn.Status == codex.TurnInProgress && h.activeTurn == "" && !containsStr(h.doneTurns, resp.Turn.ID) {
			h.activeTurn = resp.Turn.ID
		}
		h.mu.Unlock()
		return nil
	}
}

// Interrupt cancels the active turn.
func (h *handle) Interrupt(ctx context.Context) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	active := h.activeTurn
	h.mu.Unlock()
	if active == "" {
		return adapter.ErrNotActive
	}
	return c.TurnInterrupt(ctx, h.threadID, active)
}

// Stop interrupts any active turn and unsubscribes from the thread.
func (h *handle) Stop(ctx context.Context) error {
	c, err := h.client()
	if err == nil {
		h.mu.Lock()
		active := h.activeTurn
		h.mu.Unlock()
		if active != "" {
			_ = c.TurnInterrupt(ctx, h.threadID, active)
		}
		_ = c.ThreadUnsubscribe(ctx, h.threadID)
	}
	h.a.unregister(h)
	h.setStatus(model.StatusStopped, "")
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	return nil
}
