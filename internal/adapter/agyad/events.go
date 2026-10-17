// SPDX-License-Identifier: GPL-3.0-or-later

package agyad

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/ulid"
)

// turnState tracks one turn's steps.
type turnState struct {
	h     *handle
	id    string
	start time.Time
	proc  *proc.Process
	steps map[int]*step
	// streamed is true once any agent text was emitted as an item.
	streamed bool
}

// step is one step_update index within a turn.
type step struct {
	index    int
	itemID   string
	stepType string
	text     strings.Builder
	tool     map[string]any // tool_info, merged across updates
	toolName string
	started  bool
	done     bool
}

// stepUpdate is the documented step_update payload.
type stepUpdate struct {
	ConversationID  string          `json:"conversation_id"`
	StepIndex       *int            `json:"step_index"`
	State           string          `json:"state"` // ACTIVE | DONE
	StepType        string          `json:"step_type"`
	ToolName        string          `json:"tool_name"`
	TextDelta       string          `json:"text_delta"`
	DurationSeconds *float64        `json:"duration_seconds"`
	ToolInfo        map[string]any  `json:"tool_info"`
	SubagentInfo    json.RawMessage `json:"subagent_info"`
}

// result is the documented result payload.
type result struct {
	ConversationID  string   `json:"conversation_id"`
	Status          string   `json:"status"`
	Response        string   `json:"response"`
	Error           string   `json:"error"`
	DurationSeconds *float64 `json:"duration_seconds"`
	NumTurns        int      `json:"num_turns"`
	Usage           *struct {
		InputTokens     int64 `json:"input_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
		ThinkingTokens  int64 `json:"thinking_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
		TotalTokens     int64 `json:"total_tokens"`
	} `json:"usage"`
}

// parseEvent returns the event name and its payload. Documented events are
// {"event": "<name>", "<name>": {...}}; a flat {"type": ...} line (the
// shape this adapter first guessed) is still accepted.
func parseEvent(line []byte) (string, json.RawMessage, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(line, &env) != nil {
		return "", nil, false
	}
	var name string
	if raw, ok := env["event"]; ok {
		_ = json.Unmarshal(raw, &name)
	} else if raw, ok := env["type"]; ok {
		_ = json.Unmarshal(raw, &name)
	}
	if p, ok := env[name]; ok && name != "" && len(p) > 0 && p[0] == '{' {
		return name, p, true
	}
	return name, line, true
}

func (h *handle) noteConversation(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	changed := h.convID != id
	h.convID = id
	h.mu.Unlock()
	if changed {
		h.emitRegistered(false)
	}
}

// handleLine handles one line of p's stdout. Lines from a process that no
// longer owns the active turn (one still stopping after an interrupt) do not
// touch it.
func (h *handle) handleLine(p *proc.Process, line []byte) {
	name, payload, ok := parseEvent(line)
	h.mu.Lock()
	t := h.turn
	if t != nil && t.proc != p {
		t = nil
	}
	h.mu.Unlock()
	turnID := ""
	if t != nil {
		turnID = t.id
	}
	if !ok {
		h.emit(model.KindWarning, model.ErrorEvent{TurnID: turnID, Message: "agy: non-JSON output: " + truncate(string(line), 200)})
		return
	}
	switch name {
	case "init":
		var in struct {
			PermissionMode string `json:"permission_mode"`
		}
		_ = json.Unmarshal(payload, &in)
		h.mu.Lock()
		first := h.deniedSeen == nil || !h.deniedSeen["\x00init"]
		if h.deniedSeen == nil {
			h.deniedSeen = map[string]bool{}
		}
		h.deniedSeen["\x00init"] = true
		h.mu.Unlock()
		if first && in.PermissionMode != "" && in.PermissionMode != "always-proceed" && !(h.spec.MCP != nil && h.a.cfg.Options.FleetTools) {
			h.a.cfg.Logf("agy: permission mode %s: tools that need confirmation will be soft-denied", in.PermissionMode)
		}
	case "step_update":
		var su stepUpdate
		if json.Unmarshal(payload, &su) != nil {
			return
		}
		h.noteConversation(su.ConversationID)
		if t != nil {
			t.update(su)
		}
	case "agent_response": // pre-documentation guess
		var x map[string]any
		_ = json.Unmarshal(payload, &x)
		if t != nil {
			idx := 0
			su := stepUpdate{StepIndex: &idx, StepType: "agent_response", TextDelta: firstString(x, "text_delta", "delta", "text", "content")}
			t.update(su)
		}
	case "result":
		var r result
		if json.Unmarshal(payload, &r) != nil {
			return
		}
		h.noteConversation(r.ConversationID)
		if t == nil {
			return
		}
		h.mu.Lock()
		if h.turn == t {
			h.turn = nil
		}
		// The process exit that follows an interrupt finds no turn; read()
		// clears the flag then.
		interrupted := h.interrupting
		h.mu.Unlock()
		t.finish(r, interrupted)
	default:
		if t != nil {
			h.emit(model.KindItemCompleted, model.Item{TurnID: t.id, ItemID: ulid.New(), ItemType: "agy." + orDefault(name, "event"), Raw: line})
		}
	}
}

func (t *turnState) update(su stepUpdate) {
	h := t.h
	idx := 0
	if su.StepIndex != nil {
		idx = *su.StepIndex
	}
	s := t.steps[idx]
	if s == nil {
		s = &step{index: idx, itemID: t.id + "-s" + itoa(idx)}
		t.steps[idx] = s
	}
	if su.StepType != "" {
		s.stepType = su.StepType
	}
	if su.ToolName != "" {
		s.toolName = su.ToolName
	}
	if su.ToolInfo != nil {
		if s.tool == nil {
			s.tool = map[string]any{}
		}
		for k, v := range su.ToolInfo {
			s.tool[k] = v
		}
		if n, _ := su.ToolInfo["name"].(string); n != "" && s.toolName == "" {
			s.toolName = n
		}
	}
	if len(su.SubagentInfo) > 0 && string(su.SubagentInfo) != "null" {
		if s.tool == nil {
			s.tool = map[string]any{}
		}
		var sub any
		_ = json.Unmarshal(su.SubagentInfo, &sub)
		s.tool["subagent_info"] = sub
	}
	done := strings.EqualFold(su.State, "DONE")

	isText := s.stepType == "agent_response" || (s.stepType == "" && s.tool == nil && s.toolName == "")
	switch {
	case s.stepType == "user_input" || s.stepType == "checkpoint":
		// The user's message is emitted by Deliver; checkpoints are internal.
		return
	case isText:
		if su.TextDelta != "" {
			if !s.started {
				s.started = true
				t.streamed = true
				h.emit(model.KindItemStarted, model.Item{TurnID: t.id, ItemID: s.itemID, ItemType: "agentMessage"})
			}
			s.text.WriteString(su.TextDelta)
			h.emit(model.KindItemDelta, model.ItemDelta{TurnID: t.id, ItemID: s.itemID, Text: su.TextDelta})
		}
		if done && s.started && !s.done {
			s.done = true
			t.completeText(s)
		}
		return
	}
	// tool (and any other step type): one item per step
	if !s.started {
		s.started = true
		h.emit(model.KindItemStarted, t.toolItem(s, false))
	}
	if done && !s.done {
		s.done = true
		h.emit(model.KindItemCompleted, t.toolItem(s, true))
	}
}

func (t *turnState) completeText(s *step) {
	text := s.text.String()
	t.h.emit(model.KindItemCompleted, model.Item{TurnID: t.id, ItemID: s.itemID, ItemType: "agentMessage", Text: text,
		Raw: model.Marshal(map[string]any{"type": "agentMessage", "text": text})})
}

// flush completes items left open (the process ended without DONE).
func (t *turnState) flush() {
	idxs := make([]int, 0, len(t.steps))
	for i := range t.steps {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		s := t.steps[i]
		if !s.started || s.done {
			continue
		}
		s.done = true
		if s.stepType == "agent_response" || (s.stepType == "" && s.tool == nil && s.toolName == "") {
			t.completeText(s)
		} else {
			t.h.emit(model.KindItemCompleted, t.toolItem(s, true))
		}
	}
}

func (t *turnState) finish(r result, interrupted bool) {
	h := t.h
	t.flush()
	if !t.streamed && r.Response != "" {
		id := t.id + "-msg"
		h.emit(model.KindItemCompleted, model.Item{TurnID: t.id, ItemID: id, ItemType: "agentMessage", Text: r.Response,
			Raw: model.Marshal(map[string]any{"type": "agentMessage", "text": r.Response})})
	}
	if r.Usage != nil {
		h.usage(t.id, normUsage(r.Usage.InputTokens, r.Usage.CacheReadTokens, r.Usage.OutputTokens,
			r.Usage.ThinkingTokens, r.Usage.TotalTokens))
	}
	tc := model.TurnCompleted{TurnID: t.id, Status: mapStatus(r.Status)}
	// duration_seconds is for the process, which is the turn only in
	// PerTurn mode; a long-lived process reports it cumulatively.
	if r.DurationSeconds != nil && h.a.cfg.Options.PerTurn {
		tc.DurationMs = int64(*r.DurationSeconds * 1000)
	} else {
		tc.DurationMs = time.Since(t.start).Milliseconds()
	}
	if interrupted {
		tc.Status = "interrupted"
	} else if tc.Status == "failed" {
		tc.Error = r.Error
		if tc.Error == "" {
			tc.Error = "agy status " + r.Status
		}
	}
	h.emit(model.KindTurnCompleted, tc)
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if !stopped {
		h.setStatus(model.StatusIdle, "")
	}
}

// normUsage converts agy's figures to Switchworks's convention (Codex's):
// input includes cached input, and the total covers input, output and
// reasoning. agy reports input_tokens without cache reads, and thinking
// tokens apart from output.
func normUsage(input, cacheRead, output, thinking, total int64) model.Usage {
	u := model.Usage{InputTokens: input + cacheRead, CachedInputTokens: cacheRead,
		OutputTokens: output, ReasoningTokens: thinking}
	u.TotalTokens = max(total, u.InputTokens+output+thinking)
	return u
}

// usage records agy's figures. In stream-input mode they are cumulative for
// the process (the baseline resets when a process starts); a figure that did
// not grow is taken as this turn's own.
func (h *handle) usage(turnID string, rep model.Usage) {
	h.mu.Lock()
	last := rep
	if p := h.lastRep; p != nil && rep.TotalTokens > p.TotalTokens {
		last = model.Usage{
			InputTokens: rep.InputTokens - p.InputTokens, CachedInputTokens: rep.CachedInputTokens - p.CachedInputTokens,
			OutputTokens: rep.OutputTokens - p.OutputTokens, ReasoningTokens: rep.ReasoningTokens - p.ReasoningTokens,
			TotalTokens: rep.TotalTokens - p.TotalTokens,
		}
	}
	cp := rep
	h.lastRep = &cp
	h.total.InputTokens += last.InputTokens
	h.total.CachedInputTokens += last.CachedInputTokens
	h.total.OutputTokens += last.OutputTokens
	h.total.ReasoningTokens += last.ReasoningTokens
	h.total.TotalTokens += last.TotalTokens
	ev := model.UsageEvent{TurnID: turnID, Model: h.spec.Model, Last: last, Total: h.total}
	h.mu.Unlock()
	h.emit(model.KindUsage, ev)
}

func mapStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCESS", "":
		return "completed"
	case "CANCELED", "CANCELLED", "INTERRUPTED":
		return "interrupted"
	default: // ERROR, INVALID, and the non-terminal WAITING/RUNNING
		return "failed"
	}
}

// --- tool steps ---

var (
	shellTools = map[string]bool{"run_command": true, "run_shell_command": true, "shell": true, "bash": true,
		"execute_command": true, "command": true, "terminal": true, "exec": true}
	editTools = map[string]bool{"write_file": true, "write_to_file": true, "replace_file_content": true,
		"multi_replace_file_content": true, "edit_file": true, "replace": true, "create_file": true, "delete_file": true,
		"apply_patch": true, "edit": true}
)

func params(tool map[string]any) map[string]any {
	for _, k := range []string{"parameters", "args", "arguments", "input"} {
		switch v := tool[k].(type) {
		case map[string]any:
			return v
		case string:
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				return m
			}
		}
	}
	return map[string]any{}
}

func toolOutput(tool map[string]any) string {
	switch v := tool["output"].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func toolError(tool map[string]any) string {
	switch e := tool["error"].(type) {
	case map[string]any:
		msg := firstString(e, "message")
		if typ := firstString(e, "type"); typ != "" {
			return strings.TrimSpace(typ + ": " + msg)
		}
		return msg
	case string:
		return e
	}
	return ""
}

// toolItem renders a tool step in the shape of the matching Codex item
// (commandExecution, fileChange, mcpToolCall), or a generic toolCall.
func (t *turnState) toolItem(s *step, done bool) model.Item {
	name := s.toolName
	low := strings.ToLower(name)
	p := params(s.tool)
	errText := toolError(s.tool)
	status := "inProgress"
	if done {
		status = "completed"
		if errText != "" {
			status = "failed"
		}
	}
	raw := map[string]any{"id": s.itemID, "status": status, "agy": map[string]any{"step_index": s.index, "step_type": s.stepType, "tool": name}}
	itemType := "toolCall"
	switch {
	case shellTools[low]:
		itemType = "commandExecution"
		raw["command"] = firstString(p, "CommandLine", "command_line", "commandLine", "command", "cmd")
		if raw["command"] == "" {
			raw["command"] = name
		}
		if c, ok := p["Cwd"].(string); ok {
			raw["cwd"] = c
		}
		if done {
			out := toolOutput(s.tool)
			if errText != "" {
				out = strings.TrimSpace(out + "\n" + errText)
			}
			raw["aggregatedOutput"] = out
		}
	case editTools[low]:
		itemType = "fileChange"
		path := firstString(p, "TargetFile", "target_file", "AbsolutePath", "absolute_path", "path", "file_path", "filePath", "file")
		kind := "update"
		if low == "delete_file" {
			kind = "delete"
		} else if low == "write_file" || low == "write_to_file" || low == "create_file" {
			kind = "add"
		}
		changes := []map[string]string{}
		if path != "" {
			changes = append(changes, map[string]string{"path": path, "kind": kind})
		}
		raw["changes"] = changes
		if errText != "" {
			raw["error"] = errText
		}
	case strings.HasPrefix(low, "mcp") || strings.Contains(name, "/"):
		itemType = "mcpToolCall"
		server, tool := "", name
		n := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(name, "mcp("), "mcp_"), ")")
		n = strings.TrimPrefix(n, "_")
		if a, b, ok := strings.Cut(n, "/"); ok {
			server, tool = a, b
		} else if a, b, ok := strings.Cut(n, "__"); ok {
			server, tool = a, b
		} else {
			tool = n
		}
		raw["server"], raw["tool"], raw["arguments"] = server, tool, p
		if done {
			if errText != "" {
				raw["error"] = map[string]string{"message": errText}
			} else {
				raw["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": toolOutput(s.tool)}}}
			}
		}
	default:
		raw["title"], raw["tool"] = name, name
		if name == "" {
			raw["title"] = s.stepType
		}
		raw["input"] = p
		if sub, ok := s.tool["subagent_info"]; ok {
			raw["subagents"] = sub
		}
		if done {
			out := toolOutput(s.tool)
			if errText != "" {
				out = strings.TrimSpace(out + "\n" + errText)
			}
			raw["output"] = out
		}
	}
	raw["type"] = itemType
	return model.Item{TurnID: t.id, ItemID: s.itemID, ItemType: itemType, Raw: model.Marshal(raw)}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
