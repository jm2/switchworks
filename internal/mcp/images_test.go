// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jm2/switchyard/internal/fleet"
)

// The image arguments: paths are passed on to the node (which reads and
// confines them); image data from the agent and malformed lists are
// refused before anything reaches the node.
func TestImageArguments(t *testing.T) {
	for _, name := range []string{"fleet_send", "fleet_ask", "fleet_spawn"} {
		var schema map[string]any
		for _, tl := range Tools {
			if tl.Name == name {
				schema = tl.InputSchema
			}
		}
		props := schema["properties"].(map[string]any)
		img, ok := props["images"].(map[string]any)
		if !ok || img["type"] != "array" || img["maxItems"] != 4 {
			t.Fatalf("%s: images schema %v", name, props["images"])
		}
	}

	node := &stubNode{ask: func(a fleet.AskArgs) fleet.AskResult {
		return fleet.AskResult{Status: fleet.AskReplied, MessageID: "m1", Reply: "fine", ReplyFrom: "buildbox/b"}
	}}
	c := startServer(t, node)
	res := callTool(t, c, "fleet_ask", map[string]any{"to": "buildbox/b", "text": "ok?", "images": []string{"shots/a.png"}})
	if res.IsError {
		t.Fatalf("ask: %+v", res)
	}
	node.mu.Lock()
	body := node.bodies["ask"][0]
	node.mu.Unlock()
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(body, &sent)
	if string(sent["images"]) != `["shots/a.png"]` || sent["attachments"] != nil {
		t.Fatalf("forwarded %s", body)
	}

	for _, c2 := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"to": "b", "text": "x", "attachments": []any{map[string]any{"data": "AAAA"}}}, "by file path"},
		{map[string]any{"to": "b", "text": "x", "images": []string{"a", "b", "c", "d", "e"}}, "at most 4"},
		{map[string]any{"to": "b", "text": "x", "images": []string{" "}}, "file path"},
		{map[string]any{"to": "b", "text": "x", "images": "a.png"}, "bad arguments"},
	} {
		for _, tool := range []string{"fleet_send", "fleet_spawn"} {
			args := c2.args
			if tool == "fleet_spawn" {
				args = map[string]any{"prompt": "x", "images": c2.args["images"], "attachments": c2.args["attachments"]}
			}
			res := callTool(t, c, tool, args)
			if !res.IsError || !strings.Contains(res.Content[0].Text, c2.want) {
				t.Errorf("%s %v: %+v, want %q", tool, args, res, c2.want)
			}
		}
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.bodies["send"]) != 0 || len(node.bodies["spawn"]) != 0 {
		t.Fatalf("refused calls reached the node: %v", node.calls)
	}
}
