// SPDX-License-Identifier: GPL-3.0-or-later

package agyad

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
)

// The fake agy emits exactly the events and fields documented at
// https://antigravity.google/docs/cli/headless: {"event": name, name: {...}}
// lines for init, step_update (text_delta, step_type, tool_info, state) and
// result (status, response, conversation_id, usage, duration_seconds), and
// reads {"event":"user","message":{"content":...}} lines in stream-input
// mode. It logs each invocation to $SWK_FAKE_AGY_LOG.
func TestMain(m *testing.M) {
	if os.Getenv("SWK_FAKE_AGY") == "1" {
		fakeAgy()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type invocation struct {
	Argv  []string `json:"argv"`
	Token string   `json:"token"`
}

func fakeAgy() {
	args := os.Args[1:]
	conv := fmt.Sprintf("conv-%d", os.Getpid())
	stream := false
	for i, a := range args {
		switch a {
		case "--conversation":
			conv = args[i+1]
		case "--input-format":
			stream = args[i+1] == "stream-json"
		}
	}
	if p := os.Getenv("SWK_FAKE_AGY_LOG"); p != "" {
		b, _ := json.Marshal(invocation{Argv: args, Token: os.Getenv("SWK_TOKEN")})
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintf(f, "%s\n", b)
		f.Close()
	}
	emit := func(name string, payload map[string]any) {
		b, _ := json.Marshal(map[string]any{"event": name, name: payload})
		fmt.Println(string(b))
	}
	emit("init", map[string]any{"cwd": ".", "tools": []string{"run_command", "write_to_file"}, "permission_mode": "request-review"})
	turns := 0
	run := func(prompt string) bool {
		turns++
		step := 0
		su := func(p map[string]any) {
			p["conversation_id"], p["step_index"] = conv, step
			emit("step_update", p)
		}
		text := func(s string) {
			su(map[string]any{"step_type": "agent_response", "state": "ACTIVE", "text_delta": s[:len(s)/2]})
			su(map[string]any{"step_type": "agent_response", "state": "ACTIVE", "text_delta": s[len(s)/2:]})
			su(map[string]any{"step_type": "agent_response", "state": "DONE"})
			step++
		}
		tool := func(name string, params map[string]any, output string, errMsg string) {
			info := map[string]any{"name": name, "parameters": params}
			su(map[string]any{"step_type": "tool", "state": "ACTIVE", "tool_name": name, "tool_info": info})
			done := map[string]any{"name": name, "parameters": params, "output": output}
			if errMsg != "" {
				done["error"] = map[string]any{"type": "TOOL_ERROR", "message": errMsg}
			}
			su(map[string]any{"step_type": "tool", "state": "DONE", "tool_name": name, "tool_info": done})
			step++
		}
		su(map[string]any{"step_type": "user_input", "state": "DONE"})
		step++
		result := func(status, response, errText string) {
			r := map[string]any{"conversation_id": conv, "status": status, "response": response, "duration_seconds": 1.5,
				"num_turns": turns, "usage": map[string]any{"input_tokens": 10 * turns, "output_tokens": 3 * turns,
					"thinking_tokens": turns, "cache_read_tokens": 4 * turns, "total_tokens": 14 * turns}}
			if errText != "" {
				r["error"] = errText
			}
			emit("result", r)
		}
		switch {
		case strings.Contains(prompt, "SLOW"):
			text("working")
			time.Sleep(30 * time.Second)
		case strings.Contains(prompt, "CANCEL"):
			// Like real agy: a closed stdin cancels the turn, which still
			// ends with a result.
			text("working")
			_, _ = io.Copy(io.Discard, os.Stdin)
			result("ERROR", "", "interrupted")
			return true
		case strings.Contains(prompt, "NORESULT"):
			os.Exit(2)
		case strings.Contains(prompt, "FAIL"):
			result("ERROR", "", "boom")
			return true
		case strings.Contains(prompt, "TOOL"):
			tool("run_command", map[string]any{"CommandLine": "ls -1", "Cwd": "/w"}, "a\nb", "")
			tool("write_to_file", map[string]any{"TargetFile": "/w/new.txt"}, "", "")
			tool("swk/fleet_list", map[string]any{}, "2 sessions", "")
			tool("view_file", map[string]any{"AbsolutePath": "/w/a"}, "", "no such file")
			text("tools done")
			result("SUCCESS", "tools done", "")
			return true
		case strings.Contains(prompt, "DENY"):
			fmt.Fprintln(os.Stderr, "warning: tool run_command was soft-denied (requires approval)")
			text("could not run it")
			result("SUCCESS", "could not run it", "")
			return true
		case strings.Contains(prompt, "NOSTREAM"):
			result("SUCCESS", "only in the result", "")
			return true
		}
		reply := fmt.Sprintf("echo: %s (conv=%s)", prompt, conv)
		text(reply)
		result("SUCCESS", reply, "")
		return true
	}
	if !stream {
		in, _ := io.ReadAll(os.Stdin)
		run(string(in))
		return
	}
	br := bufio.NewScanner(os.Stdin)
	br.Buffer(make([]byte, 1<<20), 1<<20)
	for br.Scan() {
		var ev struct {
			Event   string `json:"event"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(br.Bytes(), &ev) != nil || ev.Event != "user" {
			emit("result", map[string]any{"status": "ERROR", "error": "bad input"})
			os.Exit(1)
		}
		run(ev.Message.Content)
	}
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

func (c *collector) all() []adapter.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]adapter.Event(nil), c.evs...)
}

func (c *collector) waitFor(t *testing.T, from int, what string, pred func(adapter.Event) bool) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs := c.all()
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				return i
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; events:%s", what, dump(c.all()))
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

func turnDone(status string) func(adapter.Event) bool {
	return func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == status
	}
}

// turn delivers text and waits for the turn's end and idle status.
func (c *collector) turn(t *testing.T, h adapter.Handle, text, status string) int {
	t.Helper()
	from := len(c.all())
	if err := h.Deliver(context.Background(), adapter.Input{Text: text}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := c.waitFor(t, from, "turn "+text, turnDone(status))
	waitIdle(t, h)
	return i
}

func items(evs []adapter.Event, kind, itemType string) []model.Item {
	var out []model.Item
	for _, e := range evs {
		if it, ok := e.Payload.(model.Item); ok && e.Kind == kind && it.ItemType == itemType {
			out = append(out, it)
		}
	}
	return out
}

func newAdapter(t *testing.T, o Options) (*Adapter, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "agy.log")
	a := New(Config{Bin: os.Args[0], Env: map[string]string{"SWK_FAKE_AGY": "1", "SWK_FAKE_AGY_LOG": log},
		Options: o, Logf: t.Logf})
	t.Cleanup(func() { a.Close() })
	return a, log
}

func invocations(t *testing.T, log string) []invocation {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []invocation
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var inv invocation
		if err := json.Unmarshal([]byte(l), &inv); err != nil {
			t.Fatal(err)
		}
		out = append(out, inv)
	}
	return out
}

func hasArgs(argv []string, want ...string) bool {
	s := " " + strings.Join(argv, " ") + " "
	return strings.Contains(s, " "+strings.Join(want, " ")+" ")
}

func TestStreamModeTurnsAndUsage(t *testing.T) {
	a, log := newAdapter(t, Options{})
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir(), Model: "gemini-3-pro"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ref().ID != "" {
		t.Fatal("conversation id must be unknown before the first turn")
	}
	col.turn(t, h, "hi", "completed")
	conv := h.Ref().ID
	if !strings.HasPrefix(conv, "conv-") {
		t.Fatalf("conversation id %q", conv)
	}
	col.waitFor(t, 0, "registration with conversation id", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionRegistered)
		return ok && p.NativeRef.ID == conv
	})
	evs := col.all()
	msgs := items(evs, model.KindItemCompleted, "agentMessage")
	if len(msgs) != 1 || msgs[0].Text != "echo: hi (conv="+conv+")" {
		t.Fatalf("messages %+v%s", msgs, dump(evs))
	}
	var deltas strings.Builder
	for _, e := range evs {
		if d, ok := e.Payload.(model.ItemDelta); ok {
			deltas.WriteString(d.Text)
		}
	}
	if deltas.String() != msgs[0].Text {
		t.Fatalf("deltas %q", deltas.String())
	}
	if u := items(evs, model.KindItemStarted, "userMessage"); len(u) != 1 {
		t.Fatalf("user items %+v", u)
	}

	// The second turn goes to the same process; agy's usage is cumulative
	// there, so the turn's own figures are the difference. Input includes
	// cache reads (agy reports them apart), and so does the total.
	col.turn(t, h, "again", "completed")
	var us []model.UsageEvent
	for _, e := range col.all() {
		if u, ok := e.Payload.(model.UsageEvent); ok {
			us = append(us, u)
		}
	}
	if len(us) != 2 || us[0].Total.TotalTokens != 18 || us[1].Last.TotalTokens != 18 || us[1].Total.TotalTokens != 36 ||
		us[1].Total.InputTokens != 28 || us[1].Total.CachedInputTokens != 8 || us[1].Total.ReasoningTokens != 2 || us[1].Model != "gemini-3-pro" {
		t.Fatalf("usage %+v", us)
	}
	inv := invocations(t, log)
	if len(inv) != 1 || !hasArgs(inv[0].Argv, "--output-format", "stream-json", "--input-format", "stream-json", "--model", "gemini-3-pro") {
		t.Fatalf("invocations %+v", inv)
	}
	if hasArgs(inv[0].Argv, "--sandbox") || hasArgs(inv[0].Argv, "--dangerously-skip-permissions") {
		t.Fatalf("unexpected sandbox flags %v", inv[0].Argv)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "x"}, adapter.DeliverSteer); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("steer: %v", err)
	}
	if err := h.Resolve(ctx, "a", "accept"); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("resolve: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "x"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("deliver after stop: %v", err)
	}
}

func TestToolSteps(t *testing.T) {
	a, _ := newAdapter(t, Options{})
	col := &collector{}
	h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.turn(t, h, "TOOL", "completed")
	evs := col.all()

	cmds := items(evs, model.KindItemCompleted, "commandExecution")
	if len(cmds) != 1 || len(items(evs, model.KindItemStarted, "commandExecution")) != 1 {
		t.Fatalf("commands%s", dump(evs))
	}
	var cmd struct{ Command, Status, AggregatedOutput, Cwd string }
	_ = json.Unmarshal(cmds[0].Raw, &cmd)
	if cmd.Command != "ls -1" || cmd.Status != "completed" || cmd.AggregatedOutput != "a\nb" || cmd.Cwd != "/w" {
		t.Fatalf("command %s", cmds[0].Raw)
	}
	fcs := items(evs, model.KindItemCompleted, "fileChange")
	if len(fcs) != 1 || !strings.Contains(string(fcs[0].Raw), `"path":"/w/new.txt"`) || !strings.Contains(string(fcs[0].Raw), `"kind":"add"`) {
		t.Fatalf("file changes %+v", fcs)
	}
	mcps := items(evs, model.KindItemCompleted, "mcpToolCall")
	var mc struct {
		Server, Tool string
		Result       struct{ Content []struct{ Text string } }
	}
	if len(mcps) == 1 {
		_ = json.Unmarshal(mcps[0].Raw, &mc)
	}
	if len(mcps) != 1 || mc.Server != "swk" || mc.Tool != "fleet_list" || mc.Result.Content[0].Text != "2 sessions" {
		t.Fatalf("mcp %+v", mcps)
	}
	gen := items(evs, model.KindItemCompleted, "toolCall")
	if len(gen) != 1 || !strings.Contains(string(gen[0].Raw), `"status":"failed"`) || !strings.Contains(string(gen[0].Raw), "TOOL_ERROR: no such file") {
		t.Fatalf("generic tool %+v", gen)
	}
	if m := items(evs, model.KindItemCompleted, "agentMessage"); len(m) != 1 || m[0].Text != "tools done" {
		t.Fatalf("message %+v", m)
	}

	// A response that was never streamed still becomes a message.
	n := len(col.all())
	col.turn(t, h, "NOSTREAM", "completed")
	if m := items(col.all()[n:], model.KindItemCompleted, "agentMessage"); len(m) != 1 || m[0].Text != "only in the result" {
		t.Fatalf("unstreamed message %+v", m)
	}
}

func TestPerTurnMode(t *testing.T) {
	a, log := newAdapter(t, Options{PerTurn: true})
	col := &collector{}
	h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.turn(t, h, "one", "completed")
	conv := h.Ref().ID
	col.turn(t, h, "two", "completed")
	inv := invocations(t, log)
	if len(inv) != 2 || hasArgs(inv[0].Argv, "--input-format") || hasArgs(inv[0].Argv, "--conversation") ||
		!hasArgs(inv[1].Argv, "--conversation", conv) {
		t.Fatalf("invocations %+v", inv)
	}
	msgs := items(col.all(), model.KindItemCompleted, "agentMessage")
	if len(msgs) != 2 || msgs[1].Text != "echo: two (conv="+conv+")" {
		t.Fatalf("messages %+v", msgs)
	}
	// Each process reports its own usage, which does not grow: per turn.
	var last model.UsageEvent
	for _, e := range col.all() {
		if u, ok := e.Payload.(model.UsageEvent); ok {
			last = u
		}
	}
	if last.Last.TotalTokens != 18 || last.Total.TotalTokens != 36 {
		t.Fatalf("usage %+v", last)
	}
}

func TestFailuresInterruptAndRestart(t *testing.T) {
	a, log := newAdapter(t, Options{})
	ctx := context.Background()
	col := &collector{}
	h, err := a.Resume(ctx, model.NativeRef{Harness: "agy", ID: "conv-7"}, adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if reg := col.all()[0].Payload.(model.SessionRegistered); !reg.Resumed || reg.NativeRef.ID != "conv-7" {
		t.Fatalf("registered %+v", reg)
	}
	i := col.turn(t, h, "FAIL", "failed")
	if tc := col.all()[i].Payload.(model.TurnCompleted); tc.Error != "boom" {
		t.Fatalf("failed turn %+v", tc)
	}
	// The process survives a failed turn.
	col.turn(t, h, "fine", "completed")
	i = col.turn(t, h, "NORESULT", "failed")
	if tc := col.all()[i].Payload.(model.TurnCompleted); !strings.Contains(tc.Error, "without a result") {
		t.Fatalf("missing result %+v", tc)
	}
	// A new process continues the conversation.
	col.turn(t, h, "after exit", "completed")

	if err := h.Deliver(ctx, adapter.Input{Text: "SLOW"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "busy"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	col.waitFor(t, 0, "slow turn streaming", func(e adapter.Event) bool {
		d, ok := e.Payload.(model.ItemDelta)
		return ok && d.Text == "wor"
	})
	if err := h.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "interrupted", turnDone("interrupted"))
	waitIdle(t, h)
	if err := h.Interrupt(ctx); !errors.Is(err, adapter.ErrNotActive) {
		t.Fatalf("idle interrupt: %v", err)
	}
	col.turn(t, h, "after interrupt", "completed")
	msgs := items(col.all(), model.KindItemCompleted, "agentMessage")
	if got := msgs[len(msgs)-1].Text; got != "echo: after interrupt (conv=conv-7)" {
		t.Fatalf("after interrupt %q", got)
	}
	inv := invocations(t, log)
	if len(inv) != 3 {
		t.Fatalf("want 3 processes (initial, after exit, after interrupt), got %d", len(inv))
	}
	for _, v := range inv {
		if !hasArgs(v.Argv, "--conversation", "conv-7") {
			t.Fatalf("process without the conversation: %v", v.Argv)
		}
	}
}

func TestSandboxFlagsAndDenials(t *testing.T) {
	a, log := newAdapter(t, Options{})
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir(), Sandbox: "workspace-write"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.turn(t, h, "DENY", "completed")
	col.waitFor(t, 0, "soft-deny warning", func(e adapter.Event) bool {
		w, ok := e.Payload.(model.ErrorEvent)
		return ok && e.Kind == model.KindWarning && strings.Contains(w.Message, "soft-denied") && strings.Contains(w.Message, "danger-full-access")
	})
	h2, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir(), Sandbox: "danger-full-access"}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	col2 := &collector{}
	_ = col2
	if err := h2.Deliver(ctx, adapter.Input{Text: "hi"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	waitIdleAfterTurn(t, h2)
	inv := invocations(t, log)
	if len(inv) != 2 || !hasArgs(inv[0].Argv, "--sandbox") || !hasArgs(inv[1].Argv, "--dangerously-skip-permissions") {
		t.Fatalf("invocations %+v", inv)
	}
}

func waitIdleAfterTurn(t *testing.T, h adapter.Handle) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.Ref().ID == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	waitIdle(t, h)
}

func TestFleetTools(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"modelProvider": "gemini", "permissions": {"allow": ["command(git)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, log := newAdapter(t, Options{FleetTools: true, SettingsPath: settings})
	repo := t.TempDir()
	fakeGitDir(t, filepath.Join(repo, ".git"))
	cwd := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(cwd, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"},
		Env: map[string]string{"SWK_TOKEN": "secret-token", "SWK_SESSION": "s1"}}
	col := &collector{}
	h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, MCP: mcp}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".agents", "mcp_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		McpServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.McpServers["swk"]
	if s.Command != os.Args[0] || len(s.Args) != 1 || s.Args[0] != "mcp" || len(s.Env) != 0 || strings.Contains(string(b), "secret-token") {
		t.Fatalf("mcp config %s", b)
	}
	ex, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if strings.TrimSpace(string(ex)) != "/sub/.agents/mcp_config.json" {
		t.Fatalf("exclude %q", ex)
	}
	sb, _ := os.ReadFile(settings)
	var sdoc map[string]any
	_ = json.Unmarshal(sb, &sdoc)
	allow := sdoc["permissions"].(map[string]any)["allow"].([]any)
	if sdoc["modelProvider"] != "gemini" || len(allow) != 2 || allow[0] != "command(git)" || allow[1] != "mcp(swk/*)" {
		t.Fatalf("settings %s", sb)
	}
	// The credentials reach agy (and so `swk mcp`) through its environment.
	col.turn(t, h, "hi", "completed")
	inv := invocations(t, log)
	if len(inv) != 1 || inv[0].Token != "secret-token" {
		t.Fatalf("invocations %+v", inv)
	}

	// A second session in the same workspace leaves everything as it is,
	// including servers the user added.
	cfgPath := filepath.Join(cwd, ".agents", "mcp_config.json")
	var full map[string]any
	_ = json.Unmarshal(b, &full)
	full["mcpServers"].(map[string]any)["mine"] = map[string]any{"command": "mine"}
	nb, _ := json.Marshal(full)
	_ = os.WriteFile(cfgPath, nb, 0o644)
	if _, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, MCP: mcp}, (&collector{}).sink); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != string(nb) {
		t.Fatalf("config rewritten:\n%s\nwas:\n%s", after, nb)
	}
	if ex2, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); string(ex2) != string(ex) {
		t.Fatalf("exclude changed: %q", ex2)
	}

	// An unreadable config is left alone, with a warning.
	bad := t.TempDir()
	_ = os.MkdirAll(filepath.Join(bad, ".agents"), 0o755)
	_ = os.WriteFile(filepath.Join(bad, ".agents", "mcp_config.json"), []byte("{oops"), 0o644)
	col3 := &collector{}
	if _, err := a.Start(context.Background(), adapter.StartSpec{Cwd: bad, MCP: mcp}, col3.sink); err != nil {
		t.Fatal(err)
	}
	col3.waitFor(t, 0, "invalid config warning", func(e adapter.Event) bool {
		w, ok := e.Payload.(model.ErrorEvent)
		return ok && strings.Contains(w.Message, "not valid JSON")
	})
	if b, _ := os.ReadFile(filepath.Join(bad, ".agents", "mcp_config.json")); string(b) != "{oops" {
		t.Fatalf("invalid config was rewritten: %q", b)
	}
}

func TestFleetToolsOffByDefault(t *testing.T) {
	a, _ := newAdapter(t, Options{})
	cwd := t.TempDir()
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"}}
	if _, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, MCP: mcp}, (&collector{}).sink); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("workspace touched without fleet_tools: %v", err)
	}
}

func waitIdle(t *testing.T, h adapter.Handle) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.Status() == model.StatusIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status %s", h.Status())
}

// fakeGitDir makes dir look enough like a git directory for gitExclude.
func fakeGitDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The workspace is the agent's: its symlinks and .git files must not steer
// Switchworks's writes elsewhere.
func TestFleetToolsWorkspaceTricks(t *testing.T) {
	outside := t.TempDir()
	cwd := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, ".agents")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if err := insideWorkspace(cwd, workspaceMCP); err == nil {
		t.Fatal("wrote through a symlinked .agents")
	}

	// A .git file pointing at an arbitrary directory is not followed.
	repo := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitExclude(repo, workspaceMCP); err == nil {
		t.Fatal("followed a gitdir that is not a git directory")
	}
	if _, err := os.Stat(filepath.Join(target, "info")); err == nil {
		t.Fatal("created directories under the bogus gitdir")
	}

	// A symlinked info directory inside a real git directory is refused.
	trap := t.TempDir()
	gd := filepath.Join(t.TempDir(), "r")
	fakeGitDir(t, filepath.Join(gd, ".git"))
	if err := os.Symlink(trap, filepath.Join(gd, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitExclude(gd, workspaceMCP); err == nil {
		t.Fatal("wrote through a symlinked info directory")
	}
	if _, err := os.Stat(filepath.Join(trap, "exclude")); err == nil {
		t.Fatal("exclude written into the symlink target")
	}

	// A worktree's excludes go to its common directory.
	main := filepath.Join(t.TempDir(), ".git")
	fakeGitDir(t, main)
	wt := filepath.Join(main, "worktrees", "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(wt, "HEAD"), []byte("ref: refs/heads/x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wt, "commondir"), []byte("../..\n"), 0o644)
	work := t.TempDir()
	_ = os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+wt+"\n"), 0o644)
	if _, _, err := gitExclude(work, workspaceMCP); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(main, "info", "exclude")); !strings.Contains(string(b), "/.agents/mcp_config.json") {
		t.Fatalf("worktree exclude: %q", b)
	}
}

func TestApprovalPolicyModes(t *testing.T) {
	for _, tc := range []struct {
		policy, sandbox string
		extra           []string
		want, notWant   []string
	}{
		{policy: "", notWant: []string{"--mode", "--dangerously-skip-permissions"}},
		{policy: "untrusted", notWant: []string{"--mode", "--dangerously-skip-permissions"}},
		{policy: "on-request", want: []string{"--mode accept-edits"}, notWant: []string{"--dangerously-skip-permissions"}},
		{policy: "on-failure", sandbox: "workspace-write", want: []string{"--sandbox", "--mode accept-edits"}},
		{policy: "never", sandbox: "workspace-write", want: []string{"--sandbox", "--dangerously-skip-permissions"}, notWant: []string{"--mode"}},
		{policy: "never", sandbox: "danger-full-access", want: []string{"--dangerously-skip-permissions"}, notWant: []string{"--mode", "--sandbox"}},
		{policy: "on-request", extra: []string{"--mode=plan"}, want: []string{"--mode=plan"}, notWant: []string{"--mode"}},
		{policy: "on-request", extra: []string{"--mode", "auto"}, want: []string{"--mode auto"}, notWant: []string{"accept-edits"}},
	} {
		a, log := newAdapter(t, Options{})
		a.cfg.ExtraArgs = tc.extra
		col := &collector{}
		h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: t.TempDir(), ApprovalPolicy: tc.policy, Sandbox: tc.sandbox}, col.sink)
		if err != nil {
			t.Fatal(err)
		}
		col.turn(t, h, "hi", "completed")
		inv := invocations(t, log)
		if len(inv) != 1 {
			t.Fatalf("%+v: invocations %+v", tc, inv)
		}
		argv := inv[0].Argv
		for _, w := range tc.want {
			if !hasArgs(argv, strings.Fields(w)...) {
				t.Errorf("%+v: missing %q in %v", tc, w, argv)
			}
		}
		for _, nw := range tc.notWant {
			if hasArgs(argv, strings.Fields(nw)...) {
				t.Errorf("%+v: unexpected %q in %v", tc, nw, argv)
			}
		}
		if strings.Count(strings.Join(argv, " "), "--dangerously-skip-permissions") > 1 {
			t.Errorf("%+v: --dangerously-skip-permissions twice in %v", tc, argv)
		}
		if strings.Count(strings.Join(argv, " "), "--mode") > 1 {
			t.Errorf("%+v: --mode twice in %v", tc, argv)
		}
	}
}

// In stream-input mode agy's duration_seconds covers the whole process, so
// the turn's own duration is measured; per turn, agy's figure is the turn's.
func TestTurnDuration(t *testing.T) {
	for _, perTurn := range []bool{false, true} {
		a, _ := newAdapter(t, Options{PerTurn: perTurn})
		col := &collector{}
		h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
		if err != nil {
			t.Fatal(err)
		}
		i := col.turn(t, h, "hi", "completed")
		d := col.all()[i].Payload.(model.TurnCompleted).DurationMs
		if perTurn && d != 1500 || !perTurn && d >= 1500 {
			t.Fatalf("per turn %v: duration %dms (agy reported 1500ms)", perTurn, d)
		}
	}
}

// agy ends a cancelled turn with a result of its own; during an interrupt
// that is an interrupted turn, not a failure, and the next turn is normal.
func TestInterruptWithResult(t *testing.T) {
	a, log := newAdapter(t, Options{})
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "CANCEL"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "cancel turn streaming", func(e adapter.Event) bool {
		d, ok := e.Payload.(model.ItemDelta)
		return ok && d.Text == "wor"
	})
	if err := h.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "interrupted", turnDone("interrupted"))
	if tc := col.all()[i].Payload.(model.TurnCompleted); tc.Error != "" {
		t.Fatalf("interrupted turn %+v", tc)
	}
	waitIdle(t, h)
	n := len(col.all())
	col.turn(t, h, "after", "completed")
	for _, e := range col.all() {
		if tc, ok := e.Payload.(model.TurnCompleted); ok && tc.Status == "failed" {
			t.Fatalf("failed turn reported:%s", dump(col.all()))
		}
	}
	if inv := invocations(t, log); len(inv) != 2 {
		t.Fatalf("want a new process after the interrupt, got %d", len(inv))
	}
	// The new process's usage starts again: the turn's own, not negative
	// or counted twice.
	var us []model.UsageEvent
	for _, e := range col.all()[n:] {
		if u, ok := e.Payload.(model.UsageEvent); ok {
			us = append(us, u)
		}
	}
	if len(us) != 1 || us[0].Last.TotalTokens != 18 || us[0].Total.TotalTokens != 36 {
		t.Fatalf("usage after interrupt %+v", us)
	}
}

// agy reports input_tokens without cache reads; Switchworks's input
// includes them.
func TestUsageNormalization(t *testing.T) {
	u := normUsage(234_000, 1_800_000, 50_000, 0, 0)
	if u.InputTokens != 2_034_000 || u.CachedInputTokens != 1_800_000 || u.OutputTokens != 50_000 || u.TotalTokens != 2_084_000 {
		t.Fatalf("usage %+v", u)
	}
	u = normUsage(10, 4, 3, 1, 14)
	if u.InputTokens != 14 || u.ReasoningTokens != 1 || u.TotalTokens != 18 {
		t.Fatalf("usage %+v", u)
	}
	// A total that already covers more is kept.
	if u = normUsage(10, 4, 3, 1, 25); u.TotalTokens != 25 {
		t.Fatalf("usage %+v", u)
	}
}

// Resuming may attach to a user's own conversation in their own repository:
// nothing is written there or to agy's settings, but the credentials still
// reach agy.
func TestFleetToolsNotSetUpOnResume(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	a, log := newAdapter(t, Options{FleetTools: true, SettingsPath: settings})
	repo := t.TempDir()
	fakeGitDir(t, filepath.Join(repo, ".git"))
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"}, Env: map[string]string{"SWK_TOKEN": "tok"}}
	col := &collector{}
	h, err := a.Resume(context.Background(), model.NativeRef{Harness: "agy", ID: "conv-9"}, adapter.StartSpec{Cwd: repo, MCP: mcp}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.turn(t, h, "hi", "completed")
	if _, err := os.Stat(filepath.Join(repo, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("workspace touched on resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "info", "exclude")); !os.IsNotExist(err) {
		t.Fatalf("exclude touched on resume: %v", err)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatalf("settings touched on resume: %v", err)
	}
	if inv := invocations(t, log); len(inv) != 1 || inv[0].Token != "tok" {
		t.Fatalf("invocations %+v", inv)
	}
}

// An explicit Stop takes out what the session's setup created, and only
// that; the global allow rule stays (with a warning when it was added).
func TestFleetToolsCleanupOnStop(t *testing.T) {
	ctx := context.Background()
	settings := filepath.Join(t.TempDir(), "settings.json")
	a, _ := newAdapter(t, Options{FleetTools: true, SettingsPath: settings})
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"}}
	repo := t.TempDir()
	fakeGitDir(t, filepath.Join(repo, ".git"))
	exFile := filepath.Join(repo, ".git", "info", "exclude")
	_ = os.MkdirAll(filepath.Dir(exFile), 0o755)
	_ = os.WriteFile(exFile, []byte("*.log\n"), 0o644)
	cfgPath := filepath.Join(repo, ".agents", "mcp_config.json")

	col := &collector{}
	h1, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, 0, "settings warning", func(e adapter.Event) bool {
		w, ok := e.Payload.(model.ErrorEvent)
		return ok && e.Kind == model.KindWarning && strings.Contains(w.Message, settings) && strings.Contains(w.Message, "mcp(swk/*)")
	})
	// A second session in the workspace takes over the cleanup when the
	// first stops.
	h2, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	_ = h1.Stop(ctx)
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config removed while another session uses it: %v", err)
	}
	_ = h2.Stop(ctx)
	if _, err := os.Stat(filepath.Join(repo, ".agents")); !os.IsNotExist(err) {
		t.Fatalf(".agents left behind: %v", err)
	}
	if b, _ := os.ReadFile(exFile); string(b) != "*.log\n" {
		t.Fatalf("exclude %q", b)
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "mcp(swk/*)") {
		t.Fatalf("global allow rule removed: %s", b)
	}

	// Servers the user added meanwhile stay.
	h3, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfgPath)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	doc["mcpServers"].(map[string]any)["mine"] = map[string]any{"command": "mine"}
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(cfgPath, b, 0o644)
	_ = h3.Stop(ctx)
	b, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(b), `"mine"`) || strings.Contains(string(b), `"swk"`) {
		t.Fatalf("config after stop %s", b)
	}

	// An entry added to a config the session did not create is removed
	// again, and the file with the user's entries stays.
	h4, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), `"swk"`) {
		t.Fatalf("entry not added to an existing config: %s", b)
	}
	_ = h4.Stop(ctx)
	if after, _ := os.ReadFile(cfgPath); strings.Contains(string(after), `"swk"`) || !strings.Contains(string(after), `"mine"`) {
		t.Fatalf("config after stop:\n%s", after)
	}

	// An swk entry that was there before the session is not the session's
	// to remove.
	doc["mcpServers"].(map[string]any)["swk"] = map[string]any{"command": os.Args[0], "args": []string{"mcp"}}
	b, _ = json.Marshal(doc)
	_ = os.WriteFile(cfgPath, b, 0o644)
	h5, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	_ = h5.Stop(ctx)
	if after, _ := os.ReadFile(cfgPath); !strings.Contains(string(after), `"swk"`) {
		t.Fatalf("pre-existing entry removed:\n%s", after)
	}
	fresh := t.TempDir()
	if _, err := a.Start(ctx, adapter.StartSpec{Cwd: fresh, MCP: mcp}, (&collector{}).sink); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := os.Stat(filepath.Join(fresh, workspaceMCP)); err != nil {
		t.Fatalf("config removed on shutdown: %v", err)
	}
}

// A different swk entry that a session replaced is put back when it stops.
func TestFleetToolsRestoreReplacedEntry(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdapter(t, Options{FleetTools: true, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"}}
	repo := t.TempDir()
	cfgPath := filepath.Join(repo, ".agents", "mcp_config.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	orig := `{"mcpServers":{"swk":{"command":"/opt/old/swk","args":["mcp","--old"]}}}`
	_ = os.WriteFile(cfgPath, []byte(orig), 0o644)
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); strings.Contains(string(b), "/opt/old/swk") {
		t.Fatalf("entry not replaced: %s", b)
	}
	_ = h.Stop(ctx)
	b, _ := os.ReadFile(cfgPath)
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.MCPServers["swk"].Command != "/opt/old/swk" || len(doc.MCPServers["swk"].Args) != 2 {
		t.Fatalf("entry not restored: %s", b)
	}
}

// An entry the user changed while the session ran is theirs: stopping
// leaves it as it is.
func TestFleetToolsKeepEditedEntry(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdapter(t, Options{FleetTools: true, SettingsPath: filepath.Join(t.TempDir(), "settings.json")})
	mcp := &adapter.MCPServer{Name: "swk", Command: os.Args[0], Args: []string{"mcp"}}
	repo := t.TempDir()
	cfgPath := filepath.Join(repo, ".agents", "mcp_config.json")
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: repo, MCP: mcp}, (&collector{}).sink)
	if err != nil {
		t.Fatal(err)
	}
	edited := `{"mcpServers":{"swk":{"command":"/usr/local/bin/swk","args":["mcp"],"disabledTools":["fleet_spawn"]}}}`
	_ = os.WriteFile(cfgPath, []byte(edited), 0o644)
	_ = h.Stop(ctx)
	if b, _ := os.ReadFile(cfgPath); string(b) != edited {
		t.Fatalf("edited entry changed on stop: %s", b)
	}
}
