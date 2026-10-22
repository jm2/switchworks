// SPDX-License-Identifier: GPL-3.0-or-later

package codexad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/codex/codextest"
	"github.com/jm2/switchworks/internal/model"
)

func TestMain(m *testing.M) {
	if codextest.MaybeRun() {
		return
	}
	restartBackoff = time.Millisecond
	os.Exit(m.Run())
}

type collector struct {
	mu  sync.Mutex
	evs []adapter.Event
}

func (c *collector) sink(e adapter.Event) {
	c.mu.Lock()
	c.evs = append(c.evs, e)
	c.mu.Unlock()
}

func (c *collector) snapshot() []adapter.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]adapter.Event(nil), c.evs...)
}

// waitFor waits until pred matches an event at index >= from, returning its index.
func (c *collector) waitFor(t *testing.T, from int, what string, pred func(adapter.Event) bool) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs := c.snapshot()
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				return i
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; events: %s", what, dump(c.snapshot()))
	return -1
}

func dump(evs []adapter.Event) string {
	var b strings.Builder
	for _, e := range evs {
		p, _ := json.Marshal(e.Payload)
		b.WriteString("\n  " + e.Kind + " " + string(p))
	}
	return b.String()
}

func kind(k string) func(adapter.Event) bool {
	return func(e adapter.Event) bool { return e.Kind == k }
}

func status(s string) func(adapter.Event) bool {
	return func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionStatus)
		return e.Kind == model.KindSessionStatus && ok && p.Status == s
	}
}

func completedText(sub string) func(adapter.Event) bool {
	return func(e adapter.Event) bool {
		it, ok := e.Payload.(model.Item)
		return e.Kind == model.KindItemCompleted && ok && it.ItemType == "agentMessage" && strings.Contains(it.Text, sub)
	}
}

func newFake(t *testing.T, state string) *Adapter {
	t.Helper()
	return newFakeWith(t, Config{Env: codextest.Env(state)})
}

func newFakeWith(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	cfg.Bin, cfg.Args, cfg.Logf = os.Args[0], []string{"app-server"}, t.Logf
	a := New(cfg)
	t.Cleanup(func() { a.Close() })
	return a
}

func TestTurnLifecycle(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Name: "devbox/test", Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	first := col.snapshot()
	if len(first) == 0 || first[0].Kind != model.KindSessionRegistered {
		t.Fatalf("first event must be session.registered: %s", dump(first))
	}
	reg := first[0].Payload.(model.SessionRegistered)
	if reg.NativeRef.Harness != "codex" || reg.NativeRef.ID == "" || reg.Model != "fake-model" {
		t.Fatalf("bad registration %+v", reg)
	}
	col.waitFor(t, 0, "idle", status(model.StatusIdle))

	if err := h.Deliver(ctx, adapter.Input{Text: "hello"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "turn.started", kind(model.KindTurnStarted))
	i = col.waitFor(t, i, "delta", kind(model.KindItemDelta))
	i = col.waitFor(t, i, "agent message", completedText("echo: hello"))
	i = col.waitFor(t, i, "usage", kind(model.KindUsage))
	col.waitFor(t, i, "turn.completed", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == "completed" && p.DurationMs == 42
	})
	u := col.snapshot()[i].Payload.(model.UsageEvent)
	if u.Total.CachedInputTokens != 40 || u.Model != "fake-model" {
		t.Fatalf("bad usage %+v", u)
	}
	// Status returns to idle so another message can start a turn.
	deadline := time.Now().Add(5 * time.Second)
	for h.Status() != model.StatusIdle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Status() != model.StatusIdle {
		t.Fatalf("status %s", h.Status())
	}
}

func TestBusySteerAndInterrupt(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "x"}, adapter.DeliverSteer); !errors.Is(err, adapter.ErrNotActive) {
		t.Fatalf("steer with no active turn: want ErrNotActive, got %v", err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "SLOW one"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "turn.started", kind(model.KindTurnStarted))
	if err := h.Deliver(ctx, adapter.Input{Text: "second"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "focus on tests"}, adapter.DeliverSteer); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "steered reply", completedText("steered: focus on tests"))
	n := col.waitFor(t, i, "first turn done", kind(model.KindTurnCompleted))

	if err := h.Deliver(ctx, adapter.Input{Text: "SLOW two"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, n, "second turn", kind(model.KindTurnStarted))
	if err := h.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, n, "interrupted", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == "interrupted"
	})
}

func TestApprovalRoundTrip(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work", ApprovalPolicy: "untrusted"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "APPROVE please"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "approval.requested", kind(model.KindApprovalRequested))
	req := col.snapshot()[i].Payload.(model.ApprovalRequested)
	if req.Command != "make test" || req.Kind != "command" {
		t.Fatalf("bad approval request %+v", req)
	}
	col.waitFor(t, 0, "waiting_approval", status(model.StatusWaitingApproval))
	if err := h.Resolve(ctx, "nope", "accept"); !errors.Is(err, adapter.ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
	if err := h.Resolve(ctx, req.ApprovalID, "bogus"); err == nil {
		t.Fatal("bad decision must be rejected")
	}
	if len(req.Decisions) != 3 || req.Decisions[0] != "accept" || req.Decisions[1] != "acceptWithExecpolicyAmendment" {
		t.Fatalf("offered decisions %v", req.Decisions)
	}
	if err := h.Resolve(ctx, req.ApprovalID, "decline"); err == nil {
		t.Fatal("a decision Codex did not offer must be rejected")
	}
	if err := h.Resolve(ctx, req.ApprovalID, "accept"); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, i, "approval.resolved", kind(model.KindApprovalResolved))
	col.waitFor(t, i, "decision reported", completedText("approval decision: accept"))
}

func TestAppServerCrashResumesThreads(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	ref := h.Ref()
	if err := h.Deliver(ctx, adapter.Input{Text: "CRASH now"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "error status", status(model.StatusError))
	i = col.waitFor(t, i, "re-registration", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionRegistered)
		return ok && p.Resumed && p.NativeRef.ID == ref.ID
	})
	col.waitFor(t, i, "idle after resume", status(model.StatusIdle))
	if err := h.Deliver(ctx, adapter.Input{Text: "after crash"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, i, "reply after resume", completedText("echo: after crash"))
}

func TestResumeAcrossAdapters(t *testing.T) {
	state := t.TempDir()
	ctx := context.Background()
	a1 := newFake(t, state)
	h1, err := a1.Start(ctx, adapter.StartSpec{Cwd: "/work"}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	ref := h1.Ref()
	a1.Close()

	a2 := newFake(t, state)
	col := &collector{}
	h2, err := a2.Resume(ctx, ref, adapter.StartSpec{Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Ref().ID != ref.ID {
		t.Fatalf("resumed %s, want %s", h2.Ref().ID, ref.ID)
	}
	if err := h2.Deliver(ctx, adapter.Input{Text: "resumed"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "reply", completedText("echo: resumed"))

	if _, err := a2.Resume(ctx, model.NativeRef{Harness: "codex", ID: "no-such-thread"}, adapter.StartSpec{}, col.sink); err == nil {
		t.Fatal("resuming an unknown thread must fail")
	}
	if err := h2.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h2.Deliver(ctx, adapter.Input{Text: "x"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("deliver after stop: %v", err)
	}
}

// A turn running when app-server dies ends as failed, so the session does
// not stay busy.
func TestAppServerCrashFailsRunningTurn(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "CRASH now"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "turn.started", kind(model.KindTurnStarted))
	started := col.snapshot()[i].Payload.(model.TurnStarted)
	col.waitFor(t, i, "failed turn", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.TurnID == started.TurnID && p.Status == "failed" && p.Error != ""
	})
}

// A session stopped while app-server is restarting stays stopped: the
// restart does not resume its thread.
func TestStopWhileAppServerRestarts(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	var mu sync.Mutex
	var h adapter.Handle
	stopped := make(chan error, 1)
	var once sync.Once
	sink := func(e adapter.Event) {
		col.sink(e)
		// Stop right when the crash is noticed, before the restart; only
		// once, so a later error status cannot block the emitting goroutine.
		if p, ok := e.Payload.(model.SessionStatus); ok && p.Status == model.StatusError {
			once.Do(func() {
				mu.Lock()
				hh := h
				mu.Unlock()
				stopped <- hh.Stop(ctx)
			})
		}
	}
	hh, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	h = hh
	mu.Unlock()
	if err := hh.Deliver(ctx, adapter.Input{Text: "CRASH now"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	stop := col.waitFor(t, 0, "stopped", status(model.StatusStopped))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := a.Info(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("app-server did not restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let the restart's resumes run
	for _, e := range col.snapshot()[stop+1:] {
		t.Errorf("event after stop: %s", dump([]adapter.Event{e}))
	}
	a.mu.Lock()
	n := len(a.threads)
	a.mu.Unlock()
	if n != 0 || hh.Status() != model.StatusStopped {
		t.Fatalf("after restart: %d thread(s) registered, status %s", n, hh.Status())
	}
}

// An app-server that keeps exiting right after it starts is restarted with
// growing waits, then left down; the log gives each exit's code.
func TestQuickExitsGiveUp(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	a := New(Config{Bin: os.Args[0], Args: []string{"app-server"}, Env: codextest.Env(t.TempDir()),
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}})
	t.Cleanup(func() { a.Close() })
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "idle", status(model.StatusIdle))
	for i := 1; i <= maxRestarts+1; i++ {
		from := len(col.snapshot())
		if err := h.Deliver(ctx, adapter.Input{Text: "CRASH now"}, adapter.DeliverQueue); err != nil {
			t.Fatalf("crash %d: %v", i, err)
		}
		if i <= maxRestarts {
			col.waitFor(t, from, fmt.Sprintf("resume after crash %d", i), status(model.StatusIdle))
		}
	}
	col.waitFor(t, 0, "giving up", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionStatus)
		return ok && p.Status == model.StatusError && strings.Contains(p.Detail, "keeps exiting")
	})
	time.Sleep(100 * time.Millisecond)
	if _, ok := a.Info(); ok {
		t.Fatal("app-server was restarted after giving up")
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(logs, "\n")
	if n := strings.Count(all, "codex app-server exited (exit code 3 after "); n != maxRestarts+1 {
		t.Fatalf("%d exits logged with their code:\n%s", n, all)
	}
	if !strings.Contains(all, fmt.Sprintf("%d times in a row within %s of starting; not restarting it", maxRestarts+1, quickExit)) {
		t.Fatalf("no give-up line:\n%s", all)
	}
}

func TestSetApprovalPolicy(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Name: "devbox/test", Cwd: "/work", ApprovalPolicy: "untrusted"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Caps().Policy {
		t.Fatal("codex sessions should report Caps.Policy")
	}
	col.waitFor(t, 0, "idle", status(model.StatusIdle))
	turn := func(from int) int {
		t.Helper()
		if err := h.Deliver(ctx, adapter.Input{Text: "POLICY"}, adapter.DeliverQueue); err != nil {
			t.Fatal(err)
		}
		i := col.waitFor(t, from, "turn.completed", kind(model.KindTurnCompleted))
		for h.Status() != model.StatusIdle {
			time.Sleep(10 * time.Millisecond)
		}
		return i
	}
	i := turn(0)
	col.waitFor(t, 0, "the spawn policy on the first turn", completedText("policy: untrusted"))
	if err := h.(adapter.PolicySetter).SetApprovalPolicy(ctx, "never"); err != nil {
		t.Fatal(err)
	}
	turn(i + 1)
	col.waitFor(t, i+1, "the new policy on the next turn", completedText("policy: never"))
}

// TestPermissionProfile checks that workspace-write sessions run under the
// configured permission profile, on start and on resume, and that other
// sandbox modes are sent as they are.
func TestPermissionProfile(t *testing.T) {
	state := t.TempDir()
	ctx := context.Background()
	sandboxOf := func(h adapter.Handle, col *collector, want string) {
		t.Helper()
		col.waitFor(t, 0, "idle", status(model.StatusIdle))
		if err := h.Deliver(ctx, adapter.Input{Text: "SANDBOX"}, adapter.DeliverQueue); err != nil {
			t.Fatal(err)
		}
		col.waitFor(t, 0, want, completedText(want))
	}

	a1 := newFakeWith(t, Config{Env: codextest.Env(state), Permissions: "swk"})
	col := &collector{}
	h, err := a1.Start(ctx, adapter.StartSpec{Name: "devbox/ww", Cwd: "/work", Sandbox: "workspace-write"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	sandboxOf(h, col, "sandbox= permissions=swk")
	ref := h.Ref()

	colRO := &collector{}
	ro, err := a1.Start(ctx, adapter.StartSpec{Name: "devbox/ro", Cwd: "/work", Sandbox: "read-only"}, colRO.sink)
	if err != nil {
		t.Fatal(err)
	}
	sandboxOf(ro, colRO, "sandbox=read-only permissions=")
	a1.Close()

	a2 := newFakeWith(t, Config{Env: codextest.Env(state), Permissions: "swk"})
	col2 := &collector{}
	h2, err := a2.Resume(ctx, ref, adapter.StartSpec{Cwd: "/work", Sandbox: "workspace-write"}, col2.sink)
	if err != nil {
		t.Fatal(err)
	}
	sandboxOf(h2, col2, "sandbox= permissions=swk")

	// Without a profile, workspace-write stays codex's sandbox mode.
	a3 := newFake(t, state)
	col3 := &collector{}
	h3, err := a3.Start(ctx, adapter.StartSpec{Name: "devbox/plain", Cwd: "/work", Sandbox: "workspace-write"}, col3.sink)
	if err != nil {
		t.Fatal(err)
	}
	sandboxOf(h3, col3, "sandbox=workspace-write permissions=")
}

// TestEffectiveSandbox checks that a thread Codex runs with another sandbox
// than requested is reported degraded, with read_only from what it runs.
func TestEffectiveSandbox(t *testing.T) {
	ctx := context.Background()
	reg := func(col *collector) model.SessionRegistered {
		t.Helper()
		i := col.waitFor(t, 0, "registered", kind(model.KindSessionRegistered))
		return col.snapshot()[i].Payload.(model.SessionRegistered)
	}
	degraded := func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionStatus)
		return ok && strings.HasPrefix(p.Detail, "degraded: ")
	}

	// As asked: no warning, and read_only only for read-only.
	a := newFake(t, t.TempDir())
	for mode, ro := range map[string]bool{"read-only": true, "workspace-write": false, "danger-full-access": false} {
		col := &collector{}
		h, err := a.Start(ctx, adapter.StartSpec{Cwd: "/work", Sandbox: mode}, col.sink)
		if err != nil {
			t.Fatal(err)
		}
		col.waitFor(t, 0, "idle", status(model.StatusIdle))
		if reg(col).Caps.ReadOnly != ro || h.Caps().ReadOnly != ro {
			t.Errorf("%s: read_only %v, want %v", mode, reg(col).Caps.ReadOnly, ro)
		}
		for _, e := range col.snapshot() {
			if e.Kind == model.KindWarning || degraded(e) {
				t.Errorf("%s: %s", mode, dump([]adapter.Event{e}))
			}
		}
	}

	// Codex on Windows without a Windows sandbox mode: workspace-write
	// runs read-only. On start and on resume.
	env := codextest.Env(t.TempDir())
	env[codextest.EnvSandbox] = "readOnly"
	b := newFakeWith(t, Config{Env: env})
	for _, resume := range []bool{false, true} {
		col := &collector{}
		spec := adapter.StartSpec{Cwd: "/work", Sandbox: "workspace-write"}
		var h adapter.Handle
		var err error
		if resume {
			h, err = b.Start(ctx, spec, (&collector{}).sink)
			if err == nil {
				ref := h.Ref()
				_ = h.Stop(ctx)
				h, err = b.Resume(ctx, ref, spec, col.sink)
			}
		} else {
			h, err = b.Start(ctx, spec, col.sink)
		}
		if err != nil {
			t.Fatal(err)
		}
		col.waitFor(t, 0, "warning", func(e adapter.Event) bool {
			p, ok := e.Payload.(model.ErrorEvent)
			return e.Kind == model.KindWarning && ok && strings.Contains(p.Message, "sandbox read-only, not the workspace-write")
		})
		col.waitFor(t, 0, "degraded idle", func(e adapter.Event) bool {
			return degraded(e) && e.Payload.(model.SessionStatus).Status == model.StatusIdle
		})
		if !reg(col).Caps.ReadOnly || !h.Caps().ReadOnly {
			t.Errorf("resume=%v: read_only false for a read-only thread", resume)
		}
	}

	// Under a permission profile only a read-only thread is a mismatch.
	for _, c := range []struct {
		requested, reported string
		profile, ro         bool
		mismatch            bool
	}{
		{"workspace-write", "workspaceWrite", true, false, false},
		{"workspace-write", "externalSandbox", true, false, false},
		{"workspace-write", "readOnly", true, true, true},
		{"workspace-write", "dangerFullAccess", false, false, true},
		{"read-only", "workspaceWrite", false, false, true},
		{"read-only", "", false, true, false},
		{"", "readOnly", false, true, false},
	} {
		raw := json.RawMessage(`{"type":"` + c.reported + `","networkAccess":false}`)
		if c.reported == "" {
			raw = nil
		}
		ro, mm := effectiveSandbox(c.requested, c.profile, raw)
		if ro != c.ro || (mm != "") != c.mismatch {
			t.Errorf("%+v: read_only %v, mismatch %q", c, ro, mm)
		}
	}
}
