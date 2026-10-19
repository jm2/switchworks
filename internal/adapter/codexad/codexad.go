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

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/codex"
	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/ulid"
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

	// Permissions names a permission profile in codex's config.toml
	// ([permissions.<id>]) for workspace-write sessions to run under in
	// place of codex's legacy workspace-write sandbox. Codex ignores a
	// profile, and its deny rules, for a thread given a sandbox mode.
	Permissions string
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
	// Its stdout closed; make sure the process and anything it started
	// are gone too.
	_ = c.Kill()
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
				// A session stopped while app-server was down stays stopped.
				if err := h.resumeOn(ctx, nc); err != nil && !errors.Is(err, adapter.ErrClosed) {
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
		if !h.isClosed() {
			h.setStatus(model.StatusError, "codex app-server could not be restarted")
		}
	}
}

// Close stops app-server.
func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	c := a.client
	a.client = nil
	hs := make([]*handle, 0, len(a.threads))
	for _, h := range a.threads {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	var err error
	if c != nil {
		err = c.Close()
	}
	// app-server is gone, so no turn can still need its image files.
	for _, h := range hs {
		h.cleanImages()
	}
	return err
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
	_ = c.ReplyError(id, jsonrpc.CodeMethodNotFound, "switchworks does not support "+method+" yet")
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
	if err := proc.CheckPinned(spec.CwdRoot, spec.Cwd); err != nil {
		return nil, err
	}
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	sandbox, perms := a.sandboxFor(spec.Sandbox)
	resp, err := c.ThreadStart(ctx, codex.ThreadStartParams{
		Cwd:            spec.Cwd,
		Model:          spec.Model,
		ModelProvider:  spec.Provider,
		ApprovalPolicy: spec.ApprovalPolicy,
		Sandbox:        sandbox,
		Permissions:    perms,
		ServiceName:    "switchworks",
		Config:         mcpConfig(spec.MCP),
	})
	if err != nil {
		return nil, fmt.Errorf("thread/start: %w", err)
	}
	h := newHandle(a, c, resp.Thread.ID, spec, sink)
	if err := h.attached(c, resp, false); err != nil {
		return nil, err
	}
	if spec.Name != "" {
		if err := c.ThreadSetName(ctx, h.threadID, spec.Name); err != nil {
			a.cfg.Logf("codex: thread/name/set: %v", err)
		}
	}
	return h, nil
}

// sandboxFor returns the sandbox mode and permission profile to send for a
// session's sandbox: the configured profile for workspace-write, if any.
func (a *Adapter) sandboxFor(mode string) (sandbox, permissions string) {
	if mode == "workspace-write" && a.cfg.Permissions != "" {
		return "", a.cfg.Permissions
	}
	return mode, ""
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

	// commandItems are the commandExecution item ids of each running
	// turn, so an interrupt can stop the commands that turn left running;
	// stopping keeps an interrupted turn's ids until that is done.
	commandItems map[string][]string
	stopping     map[string]bool

	// Image files written for localImage inputs, by turn id; removed when
	// the turn completes (app-server has read them by then) or the
	// session stops. imageMeta maps each file to its attachment, so the
	// turn's userMessage item can show the image without its path.
	scratch    string
	ownScratch bool
	files      map[string][]string
	imageMeta  map[string]model.Attachment
}

func newHandle(a *Adapter, c *codex.Client, threadID string, spec adapter.StartSpec, sink adapter.Sink) *handle {
	return &handle{
		a: a, c: c, threadID: threadID, spec: spec, sink: sink,
		status: model.StatusStarting, approvals: map[string]pendingApproval{},
		home: c.Info.CodexHome, scratch: spec.ScratchDir,
		files: map[string][]string{}, imageMeta: map[string]model.Attachment{},
	}
}

func (h *handle) emit(kind string, payload any) {
	h.sink(adapter.Event{Kind: kind, Payload: payload})
}

func (h *handle) caps() model.Caps {
	return model.Caps{Message: true, Steer: true, Interrupt: true, Stop: true, Approve: true, Images: true, Policy: true}
}

// attached finishes registration after thread/start or thread/resume. It
// returns ErrClosed, registering nothing, when the session was stopped
// meanwhile (app-server restarted under a session being stopped).
func (h *handle) attached(c *codex.Client, resp *codex.ThreadStartResponse, resumed bool) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return adapter.ErrClosed
	}
	h.c = c
	h.model = resp.Model
	h.provider = resp.ModelProvider
	h.home = c.Info.CodexHome
	h.activeTurn = ""
	h.mu.Unlock()
	early := h.a.register(h)
	// Stop marks the handle closed before it unregisters, so a Stop that
	// raced past the check above is seen here.
	if h.isClosed() {
		h.a.unregister(h)
		return adapter.ErrClosed
	}
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
	return nil
}

func (h *handle) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *handle) resumeOn(ctx context.Context, c *codex.Client) error {
	if h.isClosed() {
		return adapter.ErrClosed
	}
	if err := proc.CheckPinned(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	h.mu.Lock()
	policy := h.spec.ApprovalPolicy
	h.mu.Unlock()
	sandbox, perms := h.a.sandboxFor(h.spec.Sandbox)
	resp, err := c.ThreadResume(ctx, codex.ThreadResumeParams{
		ThreadID:       h.threadID,
		ApprovalPolicy: policy,
		Sandbox:        sandbox,
		Permissions:    perms,
		ExcludeTurns:   true,
		Config:         mcpConfig(h.spec.MCP),
	})
	if err != nil {
		return fmt.Errorf("thread/resume %s: %w", h.threadID, err)
	}
	if err := h.attached(c, resp, true); err != nil {
		// Stopped while resuming: drop the subscription thread/resume made.
		_ = c.ThreadUnsubscribe(ctx, h.threadID)
		return err
	}
	return nil
}

// lostClient ends what app-server took with it: the running turn fails,
// its pending approvals are cleared, and image files and command ids no
// turn will need are dropped.
func (h *handle) lostClient() {
	h.mu.Lock()
	h.c = nil
	turn := h.activeTurn
	h.activeTurn = ""
	if turn != "" {
		h.doneTurns = append(h.doneTurns, turn)
		if len(h.doneTurns) > 16 {
			h.doneTurns = h.doneTurns[len(h.doneTurns)-16:]
		}
	}
	var cleared []string
	for id := range h.approvals {
		cleared = append(cleared, id)
		delete(h.approvals, id)
	}
	var files []string
	for id, ps := range h.files {
		files = append(files, ps...)
		delete(h.files, id)
	}
	h.commandItems = nil
	h.stopping = nil
	h.mu.Unlock()
	h.removeFiles(files)
	for _, aid := range cleared {
		h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: aid, Decision: "cleared"})
	}
	if turn != "" {
		h.emit(model.KindTurnCompleted, model.TurnCompleted{TurnID: turn, Status: "failed", Error: "codex app-server exited"})
	}
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

// SetApprovalPolicy changes the policy the next turn starts with; a turn
// already running keeps its own.
func (h *handle) SetApprovalPolicy(_ context.Context, policy string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return adapter.ErrClosed
	}
	h.spec.ApprovalPolicy = policy
	return nil
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
			done := h.files[n.Turn.ID]
			delete(h.files, n.Turn.ID)
			if !h.stopping[n.Turn.ID] {
				delete(h.commandItems, n.Turn.ID)
			}
			h.mu.Unlock()
			h.removeFiles(done)
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
		if method == codex.NoteItemStarted && hdr.Type == "commandExecution" && n.TurnID != "" {
			h.mu.Lock()
			if h.commandItems == nil {
				h.commandItems = map[string][]string{}
			}
			h.commandItems[n.TurnID] = append(h.commandItems[n.TurnID], hdr.ID)
			h.mu.Unlock()
		}
		it := model.Item{TurnID: n.TurnID, ItemID: hdr.ID, ItemType: hdr.Type, Raw: n.Item}
		if hdr.Type == "userMessage" {
			it.Raw = h.userItem(n.Item)
		}
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
// Switchworks applies its own guards to fleet operations.
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
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchworks: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID = "command", p.TurnID, p.ItemID
		req.Command, req.Cwd, req.Reason = deref(p.Command), deref(p.Cwd), deref(p.Reason)
		req.Decisions = codex.DecisionNames(p.AvailableDecisions)
	case codex.RequestFileChangeApproval:
		var p codex.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchworks: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID, req.Reason = "file_change", p.TurnID, p.ItemID, deref(p.Reason)
		req.GrantRoot = deref(p.GrantRoot)
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
	// Codex resolves the cwd for each command, so check it before every
	// turn and steer, not only when the thread starts.
	if err := proc.CheckPinned(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	active := h.activeTurn
	status := h.status
	policy := h.spec.ApprovalPolicy
	h.mu.Unlock()
	switch mode {
	case adapter.DeliverSteer:
		if active == "" {
			return adapter.ErrNotActive
		}
		input, paths, err := h.inputs(in)
		if err != nil {
			return err
		}
		_, err = c.TurnSteer(ctx, codex.TurnSteerParams{
			ThreadID: h.threadID, ExpectedTurnID: active, Input: input,
			ClientUserMessageID: in.ClientMessageID,
		})
		h.adoptFiles(active, paths, err)
		return err
	default:
		if active != "" || status == model.StatusActive || status == model.StatusWaitingApproval {
			return adapter.ErrBusy
		}
		input, paths, err := h.inputs(in)
		if err != nil {
			return err
		}
		// Every turn carries the policy, so a change (SetApprovalPolicy)
		// takes effect with the next one.
		resp, err := c.TurnStart(ctx, codex.TurnStartParams{
			ThreadID: h.threadID, Input: input, ClientUserMessageID: in.ClientMessageID,
			ApprovalPolicy: policy,
		})
		if err != nil {
			h.adoptFiles("", paths, err)
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
		h.adoptFiles(resp.Turn.ID, paths, nil)
		return nil
	}
}

// Interrupt cancels the active turn. app-server keeps a command the turn
// was running alive as a background terminal, so the commands this turn
// started are stopped too; ones from earlier turns (a dev server, say)
// keep running.
func (h *handle) Interrupt(ctx context.Context) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	active := h.activeTurn
	if active != "" {
		if h.stopping == nil {
			h.stopping = map[string]bool{}
		}
		h.stopping[active] = true
	}
	h.mu.Unlock()
	if active == "" {
		return adapter.ErrNotActive
	}
	if err := c.TurnInterrupt(ctx, h.threadID, active); err != nil {
		h.mu.Lock()
		delete(h.stopping, active)
		h.mu.Unlock()
		return err
	}
	go h.stopTurnCommands(c, active)
	return nil
}

// stopTurnCommands terminates the background terminals started by the
// interrupted turn's commandExecution items, including ones that start
// while the turn winds down. A command becomes a background terminal as
// the turn ends, so the list is checked twice. Older app-servers without
// the background terminal methods are left as they are.
func (h *handle) stopTurnCommands(c *codex.Client, turnID string) {
	defer func() {
		h.mu.Lock()
		delete(h.stopping, turnID)
		delete(h.commandItems, turnID)
		h.mu.Unlock()
	}()
	for _, wait := range []time.Duration{500 * time.Millisecond, 3 * time.Second} {
		time.Sleep(wait)
		h.mu.Lock()
		items := append([]string(nil), h.commandItems[turnID]...)
		h.mu.Unlock()
		if len(items) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		terms, err := c.BackgroundTerminals(ctx, h.threadID)
		if err != nil {
			cancel()
			h.a.cfg.Logf("codex: list background terminals of %s: %v", h.threadID, err)
			return
		}
		for _, bt := range terms {
			if containsStr(items, bt.ItemID) {
				if err := c.TerminateBackgroundTerminal(ctx, h.threadID, bt.ProcessID); err != nil {
					h.a.cfg.Logf("codex: stop interrupted command %q: %v", bt.Command, err)
				}
			}
		}
		cancel()
	}
}

// Stop interrupts any active turn and unsubscribes from the thread.
func (h *handle) Stop(ctx context.Context) error {
	// Closed first, so a resume after an app-server restart that is under
	// way sees it (see attached) and leaves the session stopped.
	h.mu.Lock()
	c, active, was := h.c, h.activeTurn, h.closed
	h.closed = true
	h.mu.Unlock()
	if was {
		return nil
	}
	if c != nil {
		if active != "" {
			_ = c.TurnInterrupt(ctx, h.threadID, active)
		}
		// Nothing the session started should outlive it. An interrupted
		// command becomes a background terminal as its turn ends, which
		// may be after this first clean, so clean again shortly after.
		if err := c.CleanBackgroundTerminals(ctx, h.threadID); err != nil {
			h.a.cfg.Logf("codex: clean background terminals of %s: %v", h.threadID, err)
		} else if active != "" {
			go func() {
				for _, wait := range []time.Duration{time.Second, 3 * time.Second} {
					time.Sleep(wait)
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = c.CleanBackgroundTerminals(cctx, h.threadID)
					cancel()
				}
			}()
		}
		_ = c.ThreadUnsubscribe(ctx, h.threadID)
	}
	h.a.unregister(h)
	h.setStatus(model.StatusStopped, "")
	h.cleanImages()
	return nil
}
