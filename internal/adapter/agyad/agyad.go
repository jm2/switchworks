// Package agyad adapts the Antigravity CLI (`agy`) to the adapter interface.
//
// EXPERIMENTAL. It relies only on documented headless behavior
// (https://antigravity.google/docs/cli/headless):
//
//   - one `agy --output-format stream-json` process per turn, prompt on stdin
//   - `--conversation <id>` to continue a conversation, `--model`, and
//     `--dangerously-skip-permissions` (only when the session's sandbox is
//     "danger-full-access"; otherwise headless soft-denies tools that need
//     confirmation, subject to permissions.allow in agy's settings.json)
//   - stream-json emits newline-delimited JSON events whose top-level "type"
//     is init, step_update or result; result carries status, response,
//     conversation_id and usage
//
// The fields inside step_update are not documented, so the parser is tolerant:
// it pulls text out of common field names and keeps the raw event otherwise.
// Capture real streams with `swyd dev run --harness agy --record FILE` and
// tighten the parser once the schema is known.
//
// Differences from Codex: no approval relay, no mid-turn steering, interrupt
// kills the turn's process, and the conversation id is only known after the
// first turn's init/result event (session.registered is re-emitted then).
package agyad

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// Config configures the adapter.
type Config struct {
	Bin       string            // default "agy"
	ExtraArgs []string          // appended to every invocation
	Env       map[string]string // environment overrides
	Stderr    io.Writer
	Tap       jsonrpc.Tap // raw stream capture: "out" = argv + prompt, "in" = stdout lines
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
	if _, err := proc.LookPath(a.cfg.Bin); err != nil {
		return nil, err
	}
	h := a.newHandle(spec, sink, "")
	h.emitRegistered(false)
	h.setStatus(model.StatusIdle, "")
	return h, nil
}

// Resume reattaches to a conversation by id.
func (a *Adapter) Resume(ctx context.Context, ref model.NativeRef, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if ref.Harness != "" && ref.Harness != Harness {
		return nil, fmt.Errorf("agy adapter cannot resume %s session", ref.Harness)
	}
	if _, err := proc.LookPath(a.cfg.Bin); err != nil {
		return nil, err
	}
	h := a.newHandle(spec, sink, ref.ID)
	h.emitRegistered(true)
	h.setStatus(model.StatusIdle, "")
	return h, nil
}

// Close kills any running turn processes.
func (a *Adapter) Close() error {
	a.mu.Lock()
	hs := make([]*handle, 0, len(a.hs))
	for h := range a.hs {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	for _, h := range hs {
		_ = h.Stop(context.Background())
	}
	return nil
}

func (a *Adapter) newHandle(spec adapter.StartSpec, sink adapter.Sink, convID string) *handle {
	h := &handle{a: a, spec: spec, sink: sink, convID: convID, status: model.StatusStarting}
	a.mu.Lock()
	a.hs[h] = true
	a.mu.Unlock()
	return h
}

type handle struct {
	a    *Adapter
	spec adapter.StartSpec
	sink adapter.Sink

	mu           sync.Mutex
	convID       string
	status       string
	running      *proc.Process
	turnID       string
	stopped      bool
	interrupting bool
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
	if h.spec.Model != "" {
		args = append(args, "--model", h.spec.Model)
	}
	h.mu.Lock()
	if h.convID != "" {
		args = append(args, "--conversation", h.convID)
	}
	h.mu.Unlock()
	if h.spec.Sandbox == "danger-full-access" {
		args = append(args, "--dangerously-skip-permissions")
	}
	return append(args, h.a.cfg.ExtraArgs...)
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
	if h.running != nil {
		h.mu.Unlock()
		return adapter.ErrBusy
	}
	h.mu.Unlock()

	args := h.args()
	env := map[string]string{}
	for k, v := range h.a.cfg.Env {
		env[k] = v
	}
	for k, v := range h.spec.Env {
		env[k] = v
	}
	stderr := h.a.cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	p, err := proc.Start(h.a.cfg.Bin, args, proc.Options{Dir: h.spec.Cwd, Env: proc.Environ(env), Stderr: stderr})
	if err != nil {
		return fmt.Errorf("start agy: %w", err)
	}
	turnID := ulid.New()
	h.mu.Lock()
	h.running, h.turnID = p, turnID
	h.mu.Unlock()
	if tap := h.a.cfg.Tap; tap != nil {
		b, _ := json.Marshal(map[string]any{"argv": append([]string{h.a.cfg.Bin}, args...), "prompt": in.Text})
		tap("out", b)
	}
	// agy reads the prompt from stdin when stdin is not a terminal. Write it
	// before returning so an immediate Interrupt (which closes stdin) cannot
	// overtake it.
	if _, err := io.WriteString(p.Stdin, in.Text); err != nil {
		_ = p.Kill()
		h.mu.Lock()
		h.running = nil
		h.mu.Unlock()
		return fmt.Errorf("write prompt to agy: %w", err)
	}
	_ = p.Stdin.Close()
	h.setStatus(model.StatusActive, "")
	h.emit(model.KindTurnStarted, model.TurnStarted{TurnID: turnID})
	go h.readTurn(p, turnID)
	return nil
}

func (h *handle) readTurn(p *proc.Process, turnID string) {
	t := &turnState{h: h, turnID: turnID, msgID: turnID + "-msg"}
	br := bufio.NewReaderSize(p.Stdout, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if s := bytes.TrimSpace(line); len(s) > 0 {
			if tap := h.a.cfg.Tap; tap != nil {
				tap("in", s)
			}
			t.handleLine(s)
		}
		if err != nil {
			break
		}
	}
	<-p.Done()
	h.mu.Lock()
	h.running = nil
	interrupted := h.interrupting || h.stopped
	h.interrupting = false
	h.mu.Unlock()
	if !t.done {
		tc := model.TurnCompleted{TurnID: turnID, Status: "failed", Error: "agy exited without a result event"}
		if interrupted {
			tc.Status, tc.Error = "interrupted", ""
		} else if err := p.Err(); err != nil {
			tc.Error += ": " + err.Error()
		}
		h.emit(model.KindTurnCompleted, tc)
	}
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if !stopped {
		h.setStatus(model.StatusIdle, "")
	}
}

type turnState struct {
	h        *handle
	turnID   string
	msgID    string
	streamed strings.Builder
	done     bool
}

func (t *turnState) noteConversation(ev map[string]any) {
	id := firstString(ev, "conversation_id", "conversationId", "session_id", "sessionId")
	if id == "" {
		return
	}
	t.h.mu.Lock()
	changed := t.h.convID != id
	t.h.convID = id
	t.h.mu.Unlock()
	if changed {
		t.h.emitRegistered(false)
	}
}

func (t *turnState) handleLine(line []byte) {
	var ev map[string]any
	if err := json.Unmarshal(line, &ev); err != nil {
		t.h.emit(model.KindWarning, model.ErrorEvent{TurnID: t.turnID, Message: "agy: non-JSON output: " + truncate(string(line), 200)})
		return
	}
	typ, _ := ev["type"].(string)
	t.noteConversation(ev)
	switch typ {
	case "init":
	case "step_update", "agent_response":
		if text := firstString(ev, "delta", "text", "content", "message"); text != "" {
			t.streamed.WriteString(text)
			t.h.emit(model.KindItemDelta, model.ItemDelta{TurnID: t.turnID, ItemID: t.msgID, Text: text})
			return
		}
		t.h.emit(model.KindItemCompleted, model.Item{TurnID: t.turnID, ItemID: ulid.New(), ItemType: "agy." + typ, Raw: line})
	case "result":
		t.done = true
		response := firstString(ev, "response", "result", "text")
		if response == "" {
			response = t.streamed.String()
		}
		if response != "" {
			t.h.emit(model.KindItemCompleted, model.Item{TurnID: t.turnID, ItemID: t.msgID, ItemType: "agentMessage", Text: response, Raw: line})
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			usage := model.Usage{
				InputTokens:       firstInt(u, "input_tokens", "inputTokens", "prompt_tokens", "promptTokens"),
				CachedInputTokens: firstInt(u, "cached_input_tokens", "cachedInputTokens", "cached_tokens", "cachedTokens"),
				OutputTokens:      firstInt(u, "output_tokens", "outputTokens", "completion_tokens", "candidatesTokens"),
				ReasoningTokens:   firstInt(u, "reasoning_tokens", "reasoningTokens", "thoughts_tokens", "thoughtsTokens"),
				TotalTokens:       firstInt(u, "total_tokens", "totalTokens"),
			}
			if usage.TotalTokens == 0 {
				usage.TotalTokens = usage.InputTokens + usage.OutputTokens
			}
			// agy reports per-run usage; Total is per turn until a
			// conversation-level figure is known.
			t.h.emit(model.KindUsage, model.UsageEvent{TurnID: t.turnID, Model: t.h.spec.Model, Last: usage, Total: usage})
		}
		status, _ := ev["status"].(string)
		tc := model.TurnCompleted{TurnID: t.turnID, Status: mapStatus(status)}
		if d, ok := ev["duration_seconds"].(float64); ok {
			tc.DurationMs = int64(d * 1000)
		}
		if tc.Status == "failed" {
			tc.Error = firstString(ev, "error", "message")
			if tc.Error == "" {
				tc.Error = "agy status " + status
			}
		}
		t.h.emit(model.KindTurnCompleted, tc)
	default:
		t.h.emit(model.KindItemCompleted, model.Item{TurnID: t.turnID, ItemID: ulid.New(), ItemType: "agy." + orDefault(typ, "event"), Raw: line})
	}
}

func mapStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCESS", "":
		return "completed"
	case "CANCELED", "CANCELLED", "INTERRUPTED":
		return "interrupted"
	default:
		return "failed"
	}
}

// Interrupt kills the running turn.
func (h *handle) Interrupt(ctx context.Context) error {
	h.mu.Lock()
	p := h.running
	if p != nil {
		h.interrupting = true
	}
	h.mu.Unlock()
	if p == nil {
		return adapter.ErrNotActive
	}
	go func() { _ = p.Stop(4 * time.Second) }()
	return nil
}

// Resolve is not supported: headless agy never asks.
func (h *handle) Resolve(ctx context.Context, approvalID, decision string) error {
	return adapter.ErrNotSupported
}

// Stop kills any running turn and ends management.
func (h *handle) Stop(ctx context.Context) error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	p := h.running
	h.mu.Unlock()
	if p != nil {
		_ = p.Stop(4 * time.Second)
	}
	h.a.mu.Lock()
	delete(h.a.hs, h)
	h.a.mu.Unlock()
	h.setStatus(model.StatusStopped, "")
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return int64(v)
		}
	}
	return 0
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
