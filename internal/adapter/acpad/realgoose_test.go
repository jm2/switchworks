// SPDX-License-Identifier: GPL-3.0-or-later

package acpad

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/codex/codextest"
	"github.com/jm2/switchworks/internal/model"
)

// gooseEnv points a real goose at the mock provider's Anthropic Messages
// endpoint, with goose's config and sessions kept in a temporary HOME.
func gooseEnv(t *testing.T) (string, map[string]string) {
	t.Helper()
	bin := os.Getenv("SWK_GOOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose not installed (set SWK_GOOSE_BIN to run this test)")
		}
	}
	mock, err := codextest.StartMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })
	home := t.TempDir()
	noProxy := "127.0.0.1,localhost"
	return bin, map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": home + "/.config", "XDG_DATA_HOME": home + "/.local/share",
		"XDG_STATE_HOME": home + "/.local/state",
		"GOOSE_PROVIDER": "anthropic", "GOOSE_MODEL": "claude-sonnet-4-5",
		"ANTHROPIC_HOST": mock.AnthropicHost(), "ANTHROPIC_API_KEY": "mock",
		"GOOSE_DISABLE_KEYRING": "1", "GOOSE_TELEMETRY_ENABLED": "false",
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}
}

func TestRealGoose(t *testing.T) {
	bin, env := gooseEnv(t)
	cfg := GooseConfig()
	cfg.Bin, cfg.Env, cfg.Logf = bin, env, t.Logf
	a := New(cfg)
	t.Cleanup(func() { _ = a.Close() })
	cwd := t.TempDir()

	r := newRec()
	h, err := a.Start(context.Background(), adapter.StartSpec{Cwd: cwd, ApprovalPolicy: "untrusted"}, r.sink)
	if err != nil {
		t.Fatal(err)
	}
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if reg.NativeRef.Harness != Goose || reg.NativeRef.ID == "" || reg.Model != "claude-sonnet-4-5" || reg.Provider != "anthropic" {
		t.Fatalf("registered %+v", reg)
	}
	if tc := r.turn(t, h, "hello goose"); tc.Status != "completed" {
		t.Fatalf("turn %+v\n%s", tc, dump(r.all()))
	}
	msgs := items(r.all(), model.KindItemCompleted, "agentMessage")
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Text, "mock: hello goose") {
		t.Fatalf("messages %+v", msgs)
	}

	// A shell command under "untrusted" (goose's approve mode) asks first.
	n := len(r.all())
	if err := h.Deliver(context.Background(), adapter.Input{Text: "RUN: echo switchworks > out.txt && cat out.txt"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	ar := r.waitFor(t, "approval", kind(model.KindApprovalRequested)).Payload.(model.ApprovalRequested)
	if ar.Kind != "command" || ar.Command != "echo switchworks > out.txt && cat out.txt" {
		t.Fatalf("approval %+v", ar)
	}
	if err := h.Resolve(context.Background(), ar.ApprovalID, "accept"); err != nil {
		t.Fatal(err)
	}
	if tc := r.waitTurns(t, 2); tc.Status != "completed" {
		t.Fatalf("command turn %+v\n%s", tc, dump(r.all()[n:]))
	}
	cmds := items(r.all()[n:], model.KindItemCompleted, "commandExecution")
	if len(cmds) != 1 {
		t.Fatalf("command items\n%s", dump(r.all()[n:]))
	}
	var raw struct {
		Status, AggregatedOutput string
		ExitCode                 *int
	}
	_ = json.Unmarshal(cmds[0].Raw, &raw)
	if raw.Status != "completed" || strings.TrimSpace(raw.AggregatedOutput) != "switchworks" || raw.ExitCode == nil || *raw.ExitCode != 0 {
		t.Fatalf("command item %s", cmds[0].Raw)
	}
	if b, err := os.ReadFile(cwd + "/out.txt"); err != nil || strings.TrimSpace(string(b)) != "switchworks" {
		t.Fatalf("out.txt: %q %v", b, err)
	}
	var usage *model.UsageEvent
	for _, e := range r.all() {
		if u, ok := e.Payload.(model.UsageEvent); ok {
			usage = &u
		}
	}
	if usage == nil || usage.Total.TotalTokens == 0 || usage.ContextWindow == 0 {
		t.Fatalf("usage %+v", usage)
	}
	waitStatus(t, h, model.StatusIdle)

	// Reopen in a new goose process: history is not replayed into the log.
	ref := h.Ref()
	_ = a.Close()
	a2 := New(cfg)
	t.Cleanup(func() { _ = a2.Close() })
	r2 := newRec()
	h2, err := a2.Resume(context.Background(), ref, adapter.StartSpec{Cwd: cwd, ApprovalPolicy: "never"}, r2.sink)
	if err != nil {
		t.Fatal(err)
	}
	if tc := r2.turn(t, h2, "after resume"); tc.Status != "completed" {
		t.Fatalf("resumed turn %+v\n%s", tc, dump(r2.all()))
	}
	msgs = items(r2.all(), model.KindItemCompleted, "agentMessage")
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Text, "mock: after resume") {
		t.Fatalf("replay leaked into the resumed log: %+v", msgs)
	}
	// "never" maps to goose's auto mode: commands run without asking.
	n = len(r2.all())
	if err := h2.Deliver(context.Background(), adapter.Input{Text: "RUN: true"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	if tc := r2.waitTurns(t, 2); tc.Status != "completed" {
		t.Fatalf("auto turn %+v", tc)
	}
	for _, e := range r2.all()[n:] {
		if e.Kind == model.KindApprovalRequested {
			t.Fatalf("auto mode asked for approval\n%s", dump(r2.all()[n:]))
		}
	}
	if err := h2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
