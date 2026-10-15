// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/codex/codextest"
	"github.com/jm2/switchyard/internal/harness"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/node"
)

// gooseNode is a host whose only working harness is a real goose pointed
// at the mock provider's Anthropic endpoint.
type gooseNode struct {
	*runningNode
	dir string
	cfg node.Config
}

func startGooseNode(t *testing.T, c *apiclient.Client, host, bin string, mock *codextest.MockProvider) *gooseNode {
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
	home := filepath.Join(dir, "goose-home")
	noProxy := "127.0.0.1,localhost"
	cfg.Codex.Bin = "/nonexistent/codex"
	cfg.ACP = map[string]harness.ACPAgent{"goose": {Bin: bin, Env: map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": home + "/.config", "XDG_DATA_HOME": home + "/.local/share",
		"XDG_STATE_HOME": home + "/.local/state",
		"GOOSE_PROVIDER": "anthropic", "GOOSE_MODEL": "claude-sonnet-4-5",
		"ANTHROPIC_HOST": mock.AnthropicHost(), "ANTHROPIC_API_KEY": "mock",
		"GOOSE_DISABLE_KEYRING": "1", "GOOSE_TELEMETRY_ENABLED": "false",
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}}}
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "logs", "harness.log"))
			t.Logf("%s harness log:\n%s", host, b)
		}
	})
	g := &gooseNode{dir: dir, cfg: cfg}
	g.run(t)
	return g
}

// run starts (or restarts) the node from its directory.
func (g *gooseNode) run(t *testing.T) {
	t.Helper()
	hopts, err := node.HarnessOptions(g.dir, g.cfg, func(f string, a ...any) { t.Logf(g.cfg.Host+": "+f, a...) })
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(g.dir, g.cfg, hopts, hopts.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningNode{n: n, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = n.Run(ctx)
		close(r.done)
	}()
	g.runningNode = r
}

// TestRealGooseFleet runs a real goose session (over ACP) on one host and a
// fake Codex session on another. Goose calls the fleet tools that the node
// attached through session/new's mcpServers, and answers an ask routed to
// it, so messaging works across harnesses.
//
// Runs when goose is on PATH or SWYD_GOOSE_BIN is set.
func TestRealGooseFleet(t *testing.T) {
	bin := os.Getenv("SWYD_GOOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose not installed (set SWYD_GOOSE_BIN to run this test)")
		}
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
	ng := startGooseNode(t, c, "antec", bin, mock)
	defer func() { ng.stop(t) }() // ng is restarted below
	_, _, nb := enrollHost(t, c, "boxy")
	defer nb.stop(t)
	waitOnline(t, c, "antec")
	waitOnline(t, c, "boxy")

	hosts, err := c.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if h.ID == "antec" && (len(h.Adapters) != 1 || h.Adapters[0] != "goose") {
			t.Fatalf("antec adapters %v", h.Adapters)
		}
	}

	work := t.TempDir()
	lead, err := c.Spawn(ctx, model.SpawnSpec{Host: "antec", Name: "lead", Harness: "goose", Cwd: work, ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	helper, err := c.Spawn(ctx, model.SpawnSpec{Host: "boxy", Name: "helper", Cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	wl := watch(c, lead.ID)
	defer wl.cancel()
	wh := watch(c, helper.ID)
	defer wh.cancel()
	waitStatus(t, c, lead.ID, model.StatusIdle)
	waitStatus(t, c, helper.ID, model.StatusIdle)

	// goose -> fleet_list through the injected MCP server.
	if _, err := c.Send(ctx, lead.ID, call("fleet_list", map[string]any{}), "queue", ""); err != nil {
		t.Fatal(err)
	}
	i := wl.waitFor(t, "goose fleet_list", 0, agentSaidAll("tool fleet_list:", "boxy/helper", "antec/lead"))
	i = wl.waitFor(t, "goose turn done", i, turnEnded("completed"))

	// goose asks the Codex helper; the helper answers with fleet_reply.
	if _, err := c.Send(ctx, lead.ID, call("fleet_ask", map[string]any{"to": "boxy/helper", "text": "what is 6x7?", "wait_seconds": 60}), "queue", ""); err != nil {
		t.Fatal(err)
	}
	wh.waitFor(t, "helper replied", 0, agentSaidAll("replied to antec/lead"))
	i = wl.waitFor(t, "goose got the answer", i, agentSaidAll("tool fleet_ask:", "Answer from boxy/helper"))
	i = wl.waitFor(t, "goose ask turn done", i, turnEnded("completed"))

	// The Codex helper asks goose; goose answers with fleet_reply.
	j := wh.len()
	if _, err := c.Send(ctx, helper.ID, call("fleet_ask", map[string]any{"to": "antec/lead", "text": "status?", "wait_seconds": 60}), "queue", ""); err != nil {
		t.Fatal(err)
	}
	wl.waitFor(t, "goose replied", i, agentSaidAll("replied:"))
	wh.waitFor(t, "helper got goose's answer", j, agentSaidAll("Answer from antec/lead"))

	// The fleet tool calls are mcpToolCall items in goose's transcript.
	sawTool := false
	wl.mu.Lock()
	for _, e := range wl.evs {
		if e.Kind == model.KindItemCompleted && containsAll(string(e.Payload), `"item_type":"mcpToolCall"`, `"tool":"fleet_ask"`, `"server":"swyd"`) {
			sawTool = true
		}
	}
	wl.mu.Unlock()
	if !sawTool {
		t.Fatal("no mcpToolCall item for goose's fleet_ask")
	}

	// The goose session survives a node restart (session/load in a new
	// goose process).
	ng.stop(t)
	ng.run(t)
	waitOnline(t, c, "antec")
	waitStatus(t, c, lead.ID, model.StatusIdle)
	k := wl.len()
	if _, err := c.Send(ctx, lead.ID, "still there?", "queue", ""); err != nil {
		t.Fatal(err)
	}
	wl.waitFor(t, "goose after restart", k, agentSaidAll("mock: still there?"))
}
