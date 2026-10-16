// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/codex/codextest"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/node"
)

func startRealNode(t *testing.T, c *apiclient.Client, host, bin string, mock *codextest.MockProvider) *runningNode {
	t.Helper()
	tok, err := c.EnrollToken(context.Background(), host, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := node.Enroll(context.Background(), dir, tok, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := node.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Codex.Bin = bin
	hopts, err := node.HarnessOptions(dir, cfg, func(f string, a ...any) { t.Logf(host+": "+f, a...) })
	if err != nil {
		t.Fatal(err)
	}
	if hopts.CodexEnv, err = mock.WriteCodexHome(filepath.Join(dir, "codex-home")); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(dir, cfg, hopts, hopts.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningNode{n: n, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = n.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "logs", "harness.log"))
			t.Logf("%s harness log:\n%s", host, b)
		}
	})
	return r
}

// TestRealCodexFleetAsk runs two real `codex app-server`s (against the mock
// model provider) on two hosts. The model on one calls the fleet_ask tool
// that Switchworks injected over MCP; the other answers with fleet_reply.
//
// Gated because it needs the codex binary: SWK_CODEX_IT=1 go test ./...
func TestRealCodexFleetAsk(t *testing.T) {
	if os.Getenv("SWK_CODEX_IT") != "1" {
		t.Skip("set SWK_CODEX_IT=1 to run against the real codex binary")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}
	mock, err := codextest.StartMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	ctx := context.Background()
	coordDir := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer co.stop()
	c := apiClient(t, coordDir)
	na := startRealNode(t, c, "devbox", bin, mock)
	defer na.stop(t)
	nb := startRealNode(t, c, "buildbox", bin, mock)
	defer nb.stop(t)
	waitOnline(t, c, "devbox")
	waitOnline(t, c, "buildbox")

	work := t.TempDir()
	spec := func(host, name string) model.SpawnSpec {
		return model.SpawnSpec{Host: host, Name: name, Cwd: work, ApprovalPolicy: "never", Sandbox: "read-only"}
	}
	lead, err := c.Spawn(ctx, spec("devbox", "lead"))
	if err != nil {
		t.Fatal(err)
	}
	helper, err := c.Spawn(ctx, spec("buildbox", "helper"))
	if err != nil {
		t.Fatal(err)
	}
	wl := watch(c, lead.ID)
	defer wl.cancel()
	wh := watch(c, helper.ID)
	defer wh.cancel()
	waitStatus(t, c, lead.ID, model.StatusIdle)
	waitStatus(t, c, helper.ID, model.StatusIdle)

	if _, err := c.Send(ctx, lead.ID, call("fleet_ask", map[string]any{"to": "buildbox/helper", "text": "what is 6x7?", "wait_seconds": 60}), "queue", ""); err != nil {
		t.Fatal(err)
	}
	wh.waitFor(t, "helper replied via fleet_reply", 0, agentSaidAll("replied:", "reply_id"))
	i := wl.waitFor(t, "lead got the answer", 0, agentSaidAll("tool fleet_ask:", "Answer from buildbox/helper", "answer: what is 6x7?"))
	wl.waitFor(t, "lead turn done", i, turnEnded("completed"))

	// The tool call shows up as Codex's own mcpToolCall item.
	sawTool := false
	wl.mu.Lock()
	for _, e := range wl.evs {
		if e.Kind == model.KindItemCompleted && containsAll(string(e.Payload), `"item_type":"mcpToolCall"`, `"tool":"fleet_ask"`, `"server":"swk"`) {
			sawTool = true
		}
	}
	wl.mu.Unlock()
	if !sawTool {
		t.Fatal("no mcpToolCall item for fleet_ask")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
