// SPDX-License-Identifier: GPL-3.0-or-later

package acpad

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/acp/acptest"
	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/model"
)

func TestMain(m *testing.M) {
	if acptest.MaybeRun() {
		return
	}
	os.Exit(m.Run())
}

type rec struct {
	mu  sync.Mutex
	evs []adapter.Event
	ch  chan struct{}
}

func newRec() *rec { return &rec{ch: make(chan struct{}, 1)} }

func (r *rec) sink(e adapter.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

func (r *rec) all() []adapter.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]adapter.Event(nil), r.evs...)
}

// waitFor waits until an event satisfies f and returns it.
func (r *rec) waitFor(t *testing.T, what string, f func(adapter.Event) bool) adapter.Event {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for _, e := range r.all() {
			if f(e) {
				return e
			}
		}
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; events:\n%s", what, dump(r.all()))
		}
	}
}

func dump(evs []adapter.Event) string {
	var b strings.Builder
	for _, e := range evs {
		p, _ := json.Marshal(e.Payload)
		b.WriteString(e.Kind + " " + string(p) + "\n")
	}
	return b.String()
}

func kind(k string) func(adapter.Event) bool {
	return func(e adapter.Event) bool { return e.Kind == k }
}

// waitTurns waits until n turns have completed and returns the last one.
func (r *rec) waitTurns(t *testing.T, n int) model.TurnCompleted {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		var last model.TurnCompleted
		seen := 0
		for _, e := range r.all() {
			if p, ok := e.Payload.(model.TurnCompleted); ok {
				seen++
				last = p
			}
		}
		if seen >= n {
			return last
		}
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %d turns; events:\n%s", n, dump(r.all()))
		}
	}
}

func (r *rec) countTurns() int {
	n := 0
	for _, e := range r.all() {
		if e.Kind == model.KindTurnCompleted {
			n++
		}
	}
	return n
}

// turn delivers a prompt and waits for its completion event.
func (r *rec) turn(t *testing.T, h adapter.Handle, text string) model.TurnCompleted {
	t.Helper()
	n := r.countTurns()
	if err := h.Deliver(context.Background(), adapter.Input{Text: text}, adapter.DeliverQueue); err != nil {
		t.Fatalf("deliver %q: %v", text, err)
	}
	last := r.waitTurns(t, n+1)
	waitStatus(t, h, model.StatusIdle)
	return last
}

func waitStatus(t *testing.T, h adapter.Handle, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for h.Status() != want {
		if time.Now().After(deadline) {
			t.Fatalf("status %q, want %q", h.Status(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func items(evs []adapter.Event, k, itemType string) []model.Item {
	var out []model.Item
	for _, e := range evs {
		if e.Kind == k {
			if it, ok := e.Payload.(model.Item); ok && it.ItemType == itemType {
				out = append(out, it)
			}
		}
	}
	return out
}

func testAdapter(t *testing.T, extraEnv map[string]string) (*Adapter, string) {
	t.Helper()
	state := t.TempDir()
	env := acptest.Env(state)
	for k, v := range extraEnv {
		env[k] = v
	}
	a := New(Config{Harness: "fake", Bin: os.Args[0], Env: env, Logf: t.Logf})
	if os.Getenv("ACPAD_TAP") != "" {
		a.cfg.Tap = func(dir string, line []byte) { t.Logf("%s %s", dir, line) }
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, state
}

func start(t *testing.T, a *Adapter, spec adapter.StartSpec) (adapter.Handle, *rec) {
	t.Helper()
	r := newRec()
	if spec.Cwd == "" {
		spec.Cwd = t.TempDir()
	}
	h, err := a.Start(context.Background(), spec, r.sink)
	if err != nil {
		t.Fatal(err)
	}
	return h, r
}

func TestTurnStreamsAndUsage(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if reg.NativeRef.Harness != "fake" || !strings.HasPrefix(reg.NativeRef.ID, "fake_") {
		t.Fatalf("registered %+v", reg)
	}
	if reg.Caps.Steer || !reg.Caps.Approve || !reg.Caps.Interrupt {
		t.Fatalf("caps %+v", reg.Caps)
	}
	if reg.Model != "mock-small" || reg.Provider != "goose" {
		t.Fatalf("model/provider %q/%q", reg.Model, reg.Provider)
	}
	tc := r.turn(t, h, "hello")
	if tc.Status != "completed" {
		t.Fatalf("turn %+v", tc)
	}
	evs := r.all()
	msgs := items(evs, model.KindItemCompleted, "agentMessage")
	if len(msgs) != 1 || msgs[0].Text != "echo: hello" {
		t.Fatalf("messages %+v\n%s", msgs, dump(evs))
	}
	var deltas strings.Builder
	for _, e := range evs {
		if d, ok := e.Payload.(model.ItemDelta); ok {
			deltas.WriteString(d.Text)
		}
	}
	if deltas.String() != "echo: hello" {
		t.Fatalf("deltas %q", deltas.String())
	}
	if u := items(evs, model.KindItemStarted, "userMessage"); len(u) != 1 {
		t.Fatalf("user message items %d", len(u))
	}
	// Usage: the fake reports cumulative totals of 100, 200, ...
	r.turn(t, h, "again")
	var usages []model.UsageEvent
	for _, e := range r.all() {
		if u, ok := e.Payload.(model.UsageEvent); ok {
			usages = append(usages, u)
		}
	}
	if len(usages) != 2 || usages[0].Total.TotalTokens != 100 || usages[1].Last.TotalTokens != 100 ||
		usages[1].Total.TotalTokens != 200 || usages[1].ContextWindow != 128000 || usages[1].Model != "mock-small" {
		t.Fatalf("usage %+v", usages)
	}
	if err := h.Deliver(context.Background(), adapter.Input{Text: "x"}, adapter.DeliverSteer); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("steer: %v", err)
	}
}

func TestBusy(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	if err := h.Deliver(context.Background(), adapter.Input{Text: "SLOW"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(context.Background(), adapter.Input{Text: "second"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrBusy) {
		t.Fatalf("second deliver: %v", err)
	}
	if err := h.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	tc := r.waitFor(t, "interrupted", kind(model.KindTurnCompleted)).Payload.(model.TurnCompleted)
	if tc.Status != "interrupted" {
		t.Fatalf("turn %+v", tc)
	}
	waitStatus(t, h, model.StatusIdle)
	if err := h.Interrupt(context.Background()); !errors.Is(err, adapter.ErrNotActive) {
		t.Fatalf("idle interrupt: %v", err)
	}
}

func TestConfigureModeModelProviderAndMCP(t *testing.T) {
	a, state := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{Model: "mock-large", Provider: "deepseek", ApprovalPolicy: "never",
		MCP: &adapter.MCPServer{Name: "swyd", Command: os.Args[0], Args: []string{"mcp"}, Env: map[string]string{"SWYD_TOKEN": "t1"}}})
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if reg.Model != "mock-large" || reg.Provider != "deepseek" {
		t.Fatalf("registered %+v", reg)
	}
	s, err := acptest.ReadSession(state, h.Ref().ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config["mode"] != "auto" || s.Config["model"] != "mock-large" || s.Config["provider"] != "deepseek" {
		t.Fatalf("config %+v", s.Config)
	}
	var mcps []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Env     []struct{ Name, Value string }
	}
	if err := json.Unmarshal(s.McpServers, &mcps); err != nil || len(mcps) != 1 {
		t.Fatalf("mcp servers %s: %v", s.McpServers, err)
	}
	if mcps[0].Name != "swyd" || mcps[0].Args[0] != "mcp" || mcps[0].Env[0].Name != "SWYD_TOKEN" || mcps[0].Env[0].Value != "t1" {
		t.Fatalf("mcp %+v", mcps[0])
	}

	// untrusted maps to goose's "approve"; a bad model is a warning only.
	h2, r2 := start(t, a, adapter.StartSpec{Model: "no-such-model", ApprovalPolicy: "untrusted"})
	w := r2.waitFor(t, "warning", kind(model.KindWarning)).Payload.(model.ErrorEvent)
	if !strings.Contains(w.Message, "no-such-model") {
		t.Fatalf("warning %+v", w)
	}
	s2, _ := acptest.ReadSession(state, h2.Ref().ID)
	if s2.Config["mode"] != "approve" || s2.Config["model"] != "mock-small" {
		t.Fatalf("config %+v", s2.Config)
	}
	if tc := r2.turn(t, h2, "works anyway"); tc.Status != "completed" {
		t.Fatalf("turn %+v", tc)
	}
}

func TestApprovals(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	for _, c := range []struct{ decision, reply string }{
		{"accept", "approved allow_once"},
		{"acceptForSession", "approved allow_always"},
		{"decline", "declined"},
		{"declineAlways", "declined"},
	} {
		n := len(r.all())
		if err := h.Deliver(context.Background(), adapter.Input{Text: "APPROVE"}, adapter.DeliverQueue); err != nil {
			t.Fatal(err)
		}
		var ar model.ApprovalRequested
		r.waitFor(t, "approval", func(e adapter.Event) bool {
			for _, x := range r.all()[n:] {
				if p, ok := x.Payload.(model.ApprovalRequested); ok {
					ar = p
					return true
				}
			}
			return false
		})
		waitStatus(t, h, model.StatusWaitingApproval)
		if ar.Kind != "command" || ar.Command != "touch x" || strings.Join(ar.Decisions, ",") != "acceptForSession,accept,decline,declineAlways,cancel" {
			t.Fatalf("approval %+v", ar)
		}
		if err := h.Resolve(context.Background(), ar.ApprovalID, "bogus"); err == nil {
			t.Fatal("bogus decision accepted")
		}
		if err := h.Resolve(context.Background(), ar.ApprovalID, c.decision); err != nil {
			t.Fatal(err)
		}
		r.waitTurns(t, r.countTurns()+1)
		waitStatus(t, h, model.StatusIdle)
		msgs := items(r.all()[n:], model.KindItemCompleted, "agentMessage")
		if len(msgs) != 1 || msgs[0].Text != c.reply {
			t.Fatalf("%s: messages %+v", c.decision, msgs)
		}
		cmds := items(r.all()[n:], model.KindItemCompleted, "commandExecution")
		if len(cmds) != 1 {
			t.Fatalf("command items %d\n%s", len(cmds), dump(r.all()[n:]))
		}
		var raw struct {
			Command, Status, AggregatedOutput string
			ExitCode                          *int
		}
		_ = json.Unmarshal(cmds[0].Raw, &raw)
		want := "completed"
		if strings.HasPrefix(c.decision, "decline") {
			want = "failed"
		}
		if raw.Command != "touch x" || raw.Status != want {
			t.Fatalf("%s: command item %s", c.decision, cmds[0].Raw)
		}
	}
	if err := h.Resolve(context.Background(), "nope", "accept"); !errors.Is(err, adapter.ErrUnknown) {
		t.Fatalf("unknown approval: %v", err)
	}
}

func TestApprovalCancelAndInterrupt(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	// "cancel" declines and stops the turn.
	if err := h.Deliver(context.Background(), adapter.Input{Text: "APPROVE"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	ar := r.waitFor(t, "approval", kind(model.KindApprovalRequested)).Payload.(model.ApprovalRequested)
	if err := h.Resolve(context.Background(), ar.ApprovalID, "cancel"); err != nil {
		t.Fatal(err)
	}
	tc := r.waitFor(t, "turn", kind(model.KindTurnCompleted)).Payload.(model.TurnCompleted)
	if tc.Status != "interrupted" {
		t.Fatalf("turn %+v", tc)
	}
	waitStatus(t, h, model.StatusIdle)

	// Interrupt while an approval is pending clears it.
	n := len(r.all())
	if err := h.Deliver(context.Background(), adapter.Input{Text: "APPROVE"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "second approval", func(adapter.Event) bool {
		for _, e := range r.all()[n:] {
			if e.Kind == model.KindApprovalRequested {
				return true
			}
		}
		return false
	})
	if err := h.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.waitTurns(t, 2)
	cleared := false
	for _, e := range r.all()[n:] {
		if p, ok := e.Payload.(model.ApprovalResolved); ok && p.Decision == "cancel" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("approval not cleared\n%s", dump(r.all()[n:]))
	}
	waitStatus(t, h, model.StatusIdle)
}

func TestToolItems(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	r.turn(t, h, "EDIT")
	r.turn(t, h, "MCP")
	r.turn(t, h, "THINK")
	r.turn(t, h, "PLAN")
	evs := r.all()

	fc := items(evs, model.KindItemCompleted, "fileChange")
	if len(fc) != 1 {
		t.Fatalf("file changes %d\n%s", len(fc), dump(evs))
	}
	var change struct {
		Status  string
		Changes []struct{ Path, Kind, Diff string }
	}
	_ = json.Unmarshal(fc[0].Raw, &change)
	if change.Status != "completed" || len(change.Changes) != 1 || change.Changes[0].Path != "/w/main.go" ||
		!strings.Contains(change.Changes[0].Diff, "-\tprintln(\"a\")\n+\tprintln(\"b\")") ||
		!strings.Contains(change.Changes[0].Diff, "@@ -1,5 +1,5 @@") {
		t.Fatalf("file change %s", fc[0].Raw)
	}
	var diff model.TurnDiff
	for _, e := range evs {
		if d, ok := e.Payload.(model.TurnDiff); ok {
			diff = d
		}
	}
	if !strings.HasPrefix(diff.Diff, "--- a/w/main.go\n+++ b/w/main.go\n") {
		t.Fatalf("turn diff %q", diff.Diff)
	}

	mc := items(evs, model.KindItemCompleted, "mcpToolCall")
	if len(mc) != 1 {
		t.Fatalf("mcp items %d", len(mc))
	}
	var m struct {
		Server, Tool, Status string
		Result               struct{ Content []struct{ Text string } }
	}
	_ = json.Unmarshal(mc[0].Raw, &m)
	if m.Server != "swyd" || m.Tool != "fleet_list" || m.Status != "completed" || m.Result.Content[0].Text != "2 sessions" {
		t.Fatalf("mcp item %s", mc[0].Raw)
	}
	if th := items(evs, model.KindItemCompleted, "reasoning"); len(th) != 1 || th[0].Text != "hmm" {
		t.Fatalf("reasoning %+v", th)
	}
	if pl := items(evs, model.KindItemCompleted, "plan"); len(pl) != 1 || !strings.Contains(string(pl[0].Raw), "step one") {
		t.Fatalf("plan %+v", pl)
	}
}

func TestFailuresAndStopReasons(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	if tc := r.turn(t, h, "FAIL"); tc.Status != "failed" || tc.Error != "provider error: no API key" {
		t.Fatalf("fail turn %+v", tc)
	}
	if tc := r.turn(t, h, "REFUSE"); tc.Status != "failed" || !strings.Contains(tc.Error, "refused") {
		t.Fatalf("refuse turn %+v", tc)
	}
	if tc := r.turn(t, h, "fine"); tc.Status != "completed" {
		t.Fatalf("after failures %+v", tc)
	}
}

func TestResumeAcrossProcessesSuppressesReplay(t *testing.T) {
	a, state := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	r.turn(t, h, "one")
	ref, cwd := h.Ref(), ""
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	cwd = reg.Cwd
	_ = a.Close() // node shutdown: the session stays open

	a2, _ := New(Config{Harness: "fake", Bin: os.Args[0], Env: acptest.Env(state), Logf: t.Logf}), 0
	t.Cleanup(func() { _ = a2.Close() })
	r2 := newRec()
	h2, err := a2.Resume(context.Background(), ref, adapter.StartSpec{Cwd: cwd}, r2.sink)
	if err != nil {
		t.Fatal(err)
	}
	reg2 := r2.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if !reg2.Resumed || reg2.NativeRef.ID != ref.ID {
		t.Fatalf("resumed %+v", reg2)
	}
	r2.turn(t, h2, "two")
	msgs := items(r2.all(), model.KindItemCompleted, "agentMessage")
	if len(msgs) != 1 || msgs[0].Text != "echo: two" {
		t.Fatalf("replay leaked or turn missing: %+v\n%s", msgs, dump(r2.all()))
	}
	s, _ := acptest.ReadSession(state, ref.ID)
	if s.Loads != 1 || s.Closed || len(s.History) != 2 {
		t.Fatalf("fake session %+v", s)
	}
	if _, err := a2.Resume(context.Background(), model.NativeRef{Harness: "codex", ID: "x"}, adapter.StartSpec{}, newRec().sink); err == nil {
		t.Fatal("resumed a codex ref")
	}
	// Stop closes the session.
	if err := h2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h2.Status() != model.StatusStopped {
		t.Fatalf("status %s", h2.Status())
	}
	s, _ = acptest.ReadSession(state, ref.ID)
	if !s.Closed {
		t.Fatal("stop did not close the session")
	}
	if err := h2.Deliver(context.Background(), adapter.Input{Text: "x"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("deliver after stop: %v", err)
	}
}

func TestResumeOnlyAndNoLoad(t *testing.T) {
	a, state := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	r.turn(t, h, "one")
	ref := h.Ref()
	_ = a.Close()

	a2 := New(Config{Harness: "fake", Bin: os.Args[0], Env: mergeEnv(acptest.Env(state), acptest.EnvResumeOnly), Logf: t.Logf})
	t.Cleanup(func() { _ = a2.Close() })
	r2 := newRec()
	h2, err := a2.Resume(context.Background(), ref, adapter.StartSpec{}, r2.sink)
	if err != nil {
		t.Fatal(err)
	}
	if tc := r2.turn(t, h2, "two"); tc.Status != "completed" {
		t.Fatalf("turn %+v", tc)
	}
	_ = a2.Close()

	a3 := New(Config{Harness: "fake", Bin: os.Args[0], Env: mergeEnv(acptest.Env(state), acptest.EnvNoLoad), Logf: t.Logf})
	t.Cleanup(func() { _ = a3.Close() })
	if _, err := a3.Resume(context.Background(), ref, adapter.StartSpec{}, newRec().sink); err == nil ||
		!strings.Contains(err.Error(), "cannot reopen") {
		t.Fatalf("no-load resume: %v", err)
	}
}

func mergeEnv(env map[string]string, flag string) map[string]string {
	env[flag] = "1"
	return env
}

func TestCrashReopens(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	r.turn(t, h, "before")
	if err := h.Deliver(context.Background(), adapter.Input{Text: "CRASH"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	crash := r.waitTurns(t, 2)
	if crash.Status != "failed" || !strings.Contains(crash.Error, "exited during the turn") {
		t.Fatalf("crash turn %+v", crash)
	}
	// The adapter reopens the session in a new process.
	r.waitFor(t, "reopened", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionRegistered)
		return ok && p.Resumed
	})
	waitStatus(t, h, model.StatusIdle)
	if tc := r.turn(t, h, "after"); tc.Status != "completed" {
		t.Fatalf("after crash %+v", tc)
	}
}

func TestStuckAgentIsKilled(t *testing.T) {
	old := cancelGrace
	cancelGrace = 300 * time.Millisecond
	t.Cleanup(func() { cancelGrace = old })
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	if err := h.Deliver(context.Background(), adapter.Input{Text: "STUCK"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "stuck message", kind(model.KindItemDelta))
	if err := h.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	tc := r.waitFor(t, "turn end", kind(model.KindTurnCompleted)).Payload.(model.TurnCompleted)
	if tc.Status != "interrupted" {
		t.Fatalf("stuck turn %+v", tc)
	}
	r.waitFor(t, "reopened", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionRegistered)
		return ok && p.Resumed
	})
	waitStatus(t, h, model.StatusIdle)
	if tc := r.turn(t, h, "after"); tc.Status != "completed" {
		t.Fatalf("after kill %+v", tc)
	}
}

func TestUnifiedDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	nw := "a\nB\nc\nd\ne\nf\ng\nh\ni\nJ\nk\n"
	got := unifiedDiff("x.txt", &old, nw)
	want := "--- a/x.txt\n+++ b/x.txt\n@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n@@ -7,4 +7,5 @@\n g\n h\n i\n-j\n+J\n+k\n"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if got := unifiedDiff("/n.txt", nil, "x\n"); got != "--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+x\n" {
		t.Fatalf("new file diff %q", got)
	}
}

func TestFleetToolsAutoApproved(t *testing.T) {
	a, _ := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{ApprovalPolicy: "untrusted",
		MCP: &adapter.MCPServer{Name: "swyd", Command: os.Args[0], Args: []string{"mcp"}}})
	if tc := r.turn(t, h, "FLEETPERM"); tc.Status != "completed" {
		t.Fatalf("turn %+v", tc)
	}
	for _, e := range r.all() {
		if e.Kind == model.KindApprovalRequested {
			t.Fatalf("fleet tool surfaced an approval\n%s", dump(r.all()))
		}
	}
	if m := items(r.all(), model.KindItemCompleted, "agentMessage"); len(m) != 1 || m[0].Text != "fleet tool allow_once" {
		t.Fatalf("messages %+v", m)
	}
	// Without the fleet server attached, the same tool asks a person.
	h2, r2 := start(t, a, adapter.StartSpec{ApprovalPolicy: "untrusted"})
	if err := h2.Deliver(context.Background(), adapter.Input{Text: "FLEETPERM"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	r2.waitFor(t, "approval", kind(model.KindApprovalRequested))
	_ = h2.Interrupt(context.Background())
}

// A Stop or node shutdown that lands while the agent is starting must not
// leave the agent process running.
func TestCloseWhileStarting(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pids")
	a, _ := testAdapter(t, map[string]string{acptest.EnvSlowNew: "600", acptest.EnvPidFile: pids})
	errc := make(chan error, 1)
	go func() {
		_, err := a.Start(context.Background(), adapter.StartSpec{Cwd: t.TempDir()}, newRec().sink)
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond) // the agent is up; session/new is pending
	_ = a.Close()
	if err := <-errc; err == nil {
		t.Fatal("Start succeeded on a closed adapter")
	}
	b, _ := os.ReadFile(pids)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("fake agent never started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("agent process %d still running after Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session an agent spawned must not start with fewer approvals than its
// policy asks for: an unmappable policy fails the start instead of falling
// back to the agent's default mode. A person's spawn only warns.
func TestAgentSpawnApprovalFailsClosed(t *testing.T) {
	a, _ := testAdapter(t, nil)
	a.cfg.Modes = map[string]string{"untrusted": "no-such-mode"}
	cwd := t.TempDir()
	if _, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, CwdRoot: cwd, ApprovalPolicy: "untrusted"}, newRec().sink); err == nil {
		t.Fatal("an agent-spawned session started without its approval policy")
	}
	h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, ApprovalPolicy: "untrusted"}, newRec().sink)
	if err != nil {
		t.Fatalf("a person's spawn: %v", err)
	}
	_ = h.Stop(context.Background())
}
