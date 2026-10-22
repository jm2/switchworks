// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The files argument of fleet_send and fleet_ask, like images: paths go on
// to the node, file data from the agent is refused. "to" may be a list.
func TestFileArguments(t *testing.T) {
	for _, name := range []string{"fleet_send", "fleet_ask"} {
		for _, tl := range Tools {
			if tl.Name != name {
				continue
			}
			f, ok := tl.InputSchema["properties"].(map[string]any)["files"].(map[string]any)
			if !ok || f["type"] != "array" || f["maxItems"] != 4 {
				t.Fatalf("%s: files schema %v", name, f)
			}
		}
	}

	node := &stubNode{}
	c := startServer(t, node)
	// (The stub refuses every send; what it was sent is what counts.)
	callTool(t, c, "fleet_send", map[string]any{"to": []string{"buildbox/b", "devbox/c"}, "text": "am", "files": []string{"0001.patch"}})
	node.mu.Lock()
	body := node.bodies["send"][0]
	node.mu.Unlock()
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(body, &sent)
	if string(sent["files"]) != `["0001.patch"]` || string(sent["to"]) != `["buildbox/b","devbox/c"]` || sent["file_attachments"] != nil {
		t.Fatalf("forwarded %s", body)
	}

	for _, args := range []map[string]any{
		{"to": "b", "text": "x", "file_attachments": []any{map[string]any{"data": "AAAA"}}},
		{"to": "b", "text": "x", "files": []string{"a", "b", "c", "d", "e"}},
		{"to": "b", "text": "x", "files": []string{""}},
	} {
		for _, tool := range []string{"fleet_send", "fleet_ask"} {
			if res := callTool(t, c, tool, args); !res.IsError || !strings.Contains(res.Content[0].Text, "file") {
				t.Errorf("%s %v: %+v", tool, args, res)
			}
		}
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.bodies["send"]) != 1 || len(node.bodies["ask"]) != 0 {
		t.Fatalf("refused calls reached the node: %v", node.calls)
	}
}
