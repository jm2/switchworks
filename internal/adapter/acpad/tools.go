// SPDX-License-Identifier: GPL-3.0-or-later

package acpad

import (
	"encoding/json"
	"strings"

	"github.com/jm2/switchworks/internal/acp"
	"github.com/jm2/switchworks/internal/model"
)

// toolState accumulates one tool call across tool_call and
// tool_call_update notifications.
type toolState struct {
	id, itemID, turnID string

	title, name, kind, status string
	rawInput, rawOutput       json.RawMessage
	content                   []acp.ToolCallContent
	locations                 []string
	liveOutput                strings.Builder

	// Goose names the tool and its extension in _meta.
	metaTool, metaExt string

	started, completed, unfinished bool
}

func (t *toolState) merge(f acp.ToolCallFields, content []acp.ToolCallContent) {
	if f.Title != nil {
		t.title = *f.Title
	}
	if f.Name != nil {
		t.name = *f.Name
	}
	if f.Kind != nil {
		t.kind = *f.Kind
	}
	if f.Status != nil {
		t.status = *f.Status
	}
	if len(f.RawInput) > 0 && string(f.RawInput) != "null" {
		t.rawInput = f.RawInput
	}
	if len(f.RawOutput) > 0 && string(f.RawOutput) != "null" {
		t.rawOutput = f.RawOutput
	}
	if content != nil {
		t.content = content // updates replace the content list
	}
	if f.Locations != nil {
		t.locations = t.locations[:0]
		for _, l := range f.Locations {
			t.locations = append(t.locations, l.Path)
		}
	}
}

// mergeMeta reads agent-specific metadata: goose's tool and extension names
// and its streamed command output.
func (t *toolState) mergeMeta(meta json.RawMessage) {
	if len(meta) == 0 {
		return
	}
	var m struct {
		Goose struct {
			ToolCall struct {
				ToolName      string `json:"toolName"`
				ExtensionName string `json:"extensionName"`
			} `json:"toolCall"`
		} `json:"goose"`
		ToolNotification struct {
			Type   string `json:"type"`
			Params struct {
				Chunks []struct {
					Output string `json:"output"`
				} `json:"chunks"`
			} `json:"params"`
		} `json:"toolNotification"`
	}
	if json.Unmarshal(meta, &m) != nil {
		return
	}
	if n := m.Goose.ToolCall.ToolName; n != "" {
		t.metaTool, t.metaExt = n, m.Goose.ToolCall.ExtensionName
	}
	if m.ToolNotification.Type == "live_output" {
		for _, c := range m.ToolNotification.Params.Chunks {
			if t.liveOutput.Len() < 64<<10 {
				t.liveOutput.WriteString(c.Output)
			}
		}
	}
}

func (t *toolState) terminal() bool {
	return t.status == acp.ToolCompleted || t.status == acp.ToolFailed
}

// toolName is the best available machine name of the tool.
func (t *toolState) toolName() string {
	if t.metaTool != "" {
		return t.metaTool
	}
	return t.name
}

type classified struct {
	itemType     string // commandExecution | fileChange | mcpToolCall | toolCall
	command      string
	server, tool string
	paths        []string
}

var shellNames = []string{"shell", "bash", "terminal", "exec", "run_command", "execute_command", "run_shell_command"}

func inputString(raw json.RawMessage, keys ...string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
		if a, ok := m[k].([]any); ok && len(a) > 0 {
			parts := make([]string, 0, len(a))
			for _, x := range a {
				if s, ok := x.(string); ok {
					parts = append(parts, s)
				}
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}

func (t *toolState) classify() classified {
	name := t.toolName()
	low := strings.ToLower(name)

	// MCP tools: harnesses prefix them with their server ("swk__fleet_list",
	// "mcp__swk__fleet_list").
	if strings.Contains(name, "__") {
		n := strings.TrimPrefix(name, "mcp__")
		server, tool, _ := strings.Cut(n, "__")
		if t.metaExt != "" {
			server = t.metaExt
		}
		return classified{itemType: "mcpToolCall", server: server, tool: tool}
	}

	cmd := inputString(t.rawInput, "command", "cmd", "commandLine", "CommandLine")
	isShell := t.kind == acp.KindExecute
	for _, s := range shellNames {
		if low == s || strings.HasSuffix(low, "_"+s) {
			isShell = true
		}
	}
	if isShell {
		if cmd == "" {
			cmd = t.title
		}
		return classified{itemType: "commandExecution", command: cmd}
	}

	var diffs []string
	for _, c := range t.content {
		if c.Type == "diff" {
			diffs = append(diffs, c.Path)
		}
	}
	if len(diffs) > 0 || t.kind == acp.KindEdit || t.kind == acp.KindDelete || t.kind == acp.KindMove {
		paths := diffs
		if len(paths) == 0 {
			paths = append(paths, t.locations...)
		}
		if len(paths) == 0 {
			if p := inputString(t.rawInput, "path", "file_path", "filePath", "file"); p != "" {
				paths = []string{p}
			}
		}
		return classified{itemType: "fileChange", paths: paths}
	}
	return classified{itemType: "toolCall"}
}

// codexStatus maps ACP tool status onto the statuses the UI already shows.
func (t *toolState) codexStatus() string {
	switch t.status {
	case acp.ToolCompleted:
		return "completed"
	case acp.ToolFailed:
		if t.unfinished {
			return "interrupted"
		}
		return "failed"
	}
	return "inProgress"
}

// output is the tool's text result.
func (t *toolState) output() string {
	if s := acp.ToolText(t.content); s != "" {
		return s
	}
	var o struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Output string `json:"output"`
	}
	if json.Unmarshal(t.rawOutput, &o) == nil {
		if s := strings.TrimRight(o.Stdout+o.Stderr+o.Output, "\n"); s != "" {
			return s
		}
	}
	if t.liveOutput.Len() > 0 {
		return t.liveOutput.String()
	}
	var s string
	if json.Unmarshal(t.rawOutput, &s) == nil {
		return s
	}
	if len(t.rawOutput) > 0 {
		return string(t.rawOutput)
	}
	return ""
}

func (t *toolState) exitCode() *int {
	var o map[string]any
	if json.Unmarshal(t.rawOutput, &o) != nil {
		return nil
	}
	for _, k := range []string{"exit_code", "exitCode", "code"} {
		if f, ok := o[k].(float64); ok {
			n := int(f)
			return &n
		}
	}
	return nil
}

// item renders the tool call in the shape of the matching Codex item, so
// the web UI and CLI render it without special cases. Unclassified tools
// become a generic "toolCall".
func (t *toolState) item() model.Item {
	cl := t.classify()
	raw := map[string]any{"type": cl.itemType, "id": t.itemID, "status": t.codexStatus(), "acp": map[string]any{
		"toolCallId": t.id, "title": t.title, "kind": t.kind, "name": t.toolName(),
	}}
	switch cl.itemType {
	case "commandExecution":
		raw["command"] = cl.command
		if code := t.exitCode(); code != nil {
			raw["exitCode"] = *code
		}
		if t.completed {
			raw["aggregatedOutput"] = t.output()
		}
	case "mcpToolCall":
		raw["server"], raw["tool"] = cl.server, cl.tool
		args := t.rawInput
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		raw["arguments"] = args
		if t.completed {
			out := t.output()
			if t.status == acp.ToolFailed {
				raw["error"] = map[string]string{"message": out}
			} else {
				raw["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": out}}}
			}
		}
	case "fileChange":
		changes := make([]map[string]string, 0, len(cl.paths))
		byPath := map[string]acp.ToolCallContent{}
		for _, c := range t.content {
			if c.Type == "diff" {
				byPath[c.Path] = c
			}
		}
		for _, p := range cl.paths {
			ch := map[string]string{"path": p, "kind": "update"}
			if c, ok := byPath[p]; ok {
				if c.OldText == nil {
					ch["kind"] = "add"
				}
				ch["diff"] = unifiedDiff(p, c.OldText, c.NewText)
			}
			if t.kind == acp.KindDelete {
				ch["kind"] = "delete"
			}
			changes = append(changes, ch)
		}
		raw["changes"] = changes
	default:
		raw["title"], raw["kind"] = t.title, t.kind
		raw["tool"] = t.toolName()
		if len(t.rawInput) > 0 {
			raw["input"] = t.rawInput
		}
		if len(t.locations) > 0 {
			raw["locations"] = t.locations
		}
		if t.completed {
			raw["output"] = t.output()
		}
	}
	return model.Item{TurnID: t.turnID, ItemID: t.itemID, ItemType: cl.itemType, Raw: model.Marshal(raw)}
}

// diffText is the unified diff of the call's file edits.
func (t *toolState) diffText() string {
	var b strings.Builder
	for _, c := range t.content {
		if c.Type == "diff" {
			b.WriteString(unifiedDiff(c.Path, c.OldText, c.NewText))
		}
	}
	return b.String()
}
