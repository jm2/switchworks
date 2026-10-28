// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
)

func TestLockTools(t *testing.T) {
	node := &stubNode{}
	c := startServer(t, node)
	var init struct {
		Instructions string `json:"instructions"`
	}
	if err := c.Call(context.Background(), "initialize", map[string]any{"protocolVersion": "2025-06-18"}, &init); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(init.Instructions, "fleet_lock") {
		t.Fatalf("instructions do not mention fleet_lock: %s", init.Instructions)
	}
	var tl struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.Call(context.Background(), "tools/list", map[string]any{}, &tl); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tl.Tools {
		names[tool.Name] = true
	}
	for _, n := range []string{"fleet_lock", "fleet_unlock", "fleet_locks", "fleet_list"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}

	for _, c2 := range []struct {
		tool, op string
		args     map[string]any
	}{
		{"fleet_lock", "lock", map[string]any{"name": "host:x:gpu"}},
		{"fleet_unlock", "unlock", map[string]any{"name": "host:x:gpu"}},
		{"fleet_locks", "locks", map[string]any{"prefix": "host:x"}},
	} {
		if res := callTool(t, c, c2.tool, c2.args); res.IsError {
			t.Fatalf("%s: %+v", c2.tool, res)
		}
		node.mu.Lock()
		n := len(node.bodies[c2.op])
		node.mu.Unlock()
		if n != 1 {
			t.Errorf("%s: %d calls to %s", c2.tool, n, c2.op)
		}
	}
	res := callTool(t, c, "fleet_lock", map[string]any{"name": "gpu", "wait_seconds": 30, "ttl_seconds": 60, "note": "bench"})
	if res.IsError {
		t.Fatalf("fleet_lock: %+v", res)
	}
	node.mu.Lock()
	var a fleet.LockArgs
	err := json.Unmarshal(node.bodies["lock"][1], &a)
	node.mu.Unlock()
	if err != nil || a != (fleet.LockArgs{Name: "gpu", TTLSeconds: 60, WaitSeconds: 30, Note: "bench"}) {
		t.Fatalf("forwarded %+v %v", a, err)
	}
	if res := callTool(t, c, "fleet_lock", map[string]any{"name": "gpu", "wait_seconds": -1}); !res.IsError {
		t.Fatal("negative wait accepted")
	}
}
