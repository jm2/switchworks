// SPDX-License-Identifier: GPL-3.0-or-later

package codextest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The mock also serves the Anthropic Messages API at POST /v1/messages, the
// wire format DeepSeek, Z.ai GLM and Xiaomi MiMo all offer natively. It
// drives harnesses other than Codex (Goose over ACP) with the same script:
//
//   - "RUN: <cmd>", or a line "$ <cmd>": call the harness's shell tool (the first advertised tool
//     whose name is or ends in "shell", "exec_command" or "bash") with the
//     command in whichever of "command"/"cmd" its schema declares
//   - a line "CALL <tool> <json args>": call the advertised tool named <tool>
//     or ending in "__<tool>" / "_<tool>" (MCP tools are prefixed by the
//     harness), then answer "tool <tool>: <output>"
//   - a Switchyard ask: answer it with fleet_reply, as for Codex
//   - anything else: "mock: <message>" in three deltas
//   - with image blocks (base64 sources): as for the Responses API, the
//     vision probe's code, or each image's type, size and SHA-256
//
// AnthropicHost is the base URL without /v1, as ANTHROPIC_HOST expects.
func (m *MockProvider) AnthropicHost() string { return strings.TrimSuffix(m.URL, "/v1") }

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    struct {
		Type      string `json:"type"` // base64
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"` // image
}

type anthTool struct {
	Name        string `json:"name"`
	InputSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"input_schema"`
}

// blocks decodes a message's content: a string or a list of blocks.
func blocks(raw json.RawMessage) []anthBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anthBlock{{Type: "text", Text: s}}
	}
	var bs []anthBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func toolResultText(raw json.RawMessage) string {
	var out []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

func findTool(tools []anthTool, match func(string) bool) *anthTool {
	for i := range tools {
		if match(tools[i].Name) {
			return &tools[i]
		}
	}
	return nil
}

func (m *MockProvider) serveAnthropic(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string        `json:"model"`
		Messages []anthMessage `json:"messages"`
		Tools    []anthTool    `json:"tools"`
		Stream   bool          `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.reqs++
	n := m.reqs
	m.mu.Unlock()

	// The last user message is either text (a new prompt) or tool results.
	lastUser, sawOutput, output := "", false, ""
	var images [][]byte
	for _, msg := range body.Messages {
		if msg.Role != "user" {
			continue
		}
		var texts []string
		var imgs [][]byte
		results := false
		for _, b := range blocks(msg.Content) {
			switch b.Type {
			case "image":
				if b.Source.Type == "base64" {
					if d, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
						imgs = append(imgs, d)
					}
				}
			case "text":
				// Harnesses append their own context (goose: a
				// <turn-context> block); the script reads the user's words.
				if t := strings.TrimSpace(b.Text); strings.HasPrefix(t, "<turn-context>") || strings.HasPrefix(t, "---BEGIN HINTS") {
					continue
				}
				if i := strings.Index(b.Text, "\n<turn-context>"); i >= 0 {
					b.Text = b.Text[:i]
				}
				texts = append(texts, b.Text)
			case "tool_result":
				results = true
				output = toolResultText(b.Content)
			}
		}
		if results {
			sawOutput = true
		} else if len(texts) > 0 {
			lastUser, sawOutput, images = strings.Join(texts, "\n"), false, imgs
		} else if len(imgs) > 0 {
			images = append(images, imgs...)
		}
	}
	seen, withImages := SeeImages(lastUser, images)

	type toolCall struct {
		name  string
		input map[string]any
	}
	var call *toolCall
	text := ""
	askM := askRe.FindStringSubmatch(lastUser)
	callM := callRe.FindStringSubmatch(lastUser)
	suffixed := func(tool string) func(string) bool {
		return func(n string) bool {
			return n == tool || strings.HasSuffix(n, "__"+tool) || strings.HasSuffix(n, "_"+tool)
		}
	}
	switch {
	case !sawOutput && askM != nil:
		if t := findTool(body.Tools, suffixed("fleet_reply")); t != nil {
			lines := strings.Split(strings.TrimSpace(lastUser), "\n")
			call = &toolCall{t.Name, map[string]any{"message_id": askM[1], "text": "answer: " + lines[len(lines)-1]}}
		} else {
			text = "mock: no fleet_reply tool"
		}
	case !sawOutput && callM != nil:
		if t := findTool(body.Tools, suffixed(callM[1])); t != nil {
			args := map[string]any{}
			if s := strings.TrimSpace(callM[2]); s != "" {
				_ = json.Unmarshal([]byte(s), &args)
			}
			call = &toolCall{t.Name, args}
		} else {
			text = "mock: no tool " + callM[1] + " among " + toolNames(body.Tools)
		}
	case !sawOutput && shellCommand(lastUser) != "":
		t := findTool(body.Tools, func(n string) bool {
			return n == "shell" || n == "bash" || n == "exec_command" ||
				strings.HasSuffix(n, "__shell") || strings.HasSuffix(n, "__bash")
		})
		if t == nil {
			text = "mock: no shell tool among " + toolNames(body.Tools)
			break
		}
		key := "command"
		if _, ok := t.InputSchema.Properties["command"]; !ok {
			if _, ok := t.InputSchema.Properties["cmd"]; ok {
				key = "cmd"
			}
		}
		call = &toolCall{t.Name, map[string]any{key: shellCommand(lastUser)}}
	case sawOutput && askM != nil:
		text = "replied: " + output
	case sawOutput && callM != nil:
		text = "tool " + callM[1] + ": " + output
	case sawOutput:
		text = "Command finished."
	case withImages && strings.Contains(strings.ToLower(lastUser), "code in this image"):
		text = seen
	default:
		text = "mock: " + lastUser + seen
	}

	model := body.Model
	if model == "" {
		model = "mock-model"
	}
	msgID := fmt.Sprintf("msg_mock_%d", n)
	usage := map[string]any{"input_tokens": 120, "cache_read_input_tokens": 40, "output_tokens": 7}
	stop := "end_turn"
	var content []any
	if call != nil {
		stop = "tool_use"
		content = []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", n), "name": call.name, "input": call.input}}
	} else {
		content = []any{map[string]any{"type": "text", "text": text}}
	}

	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	ev := func(typ string, v map[string]any) {
		v["type"] = typ
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev("message_start", map[string]any{"message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 120, "cache_read_input_tokens": 40, "output_tokens": 1}}})
	if call != nil {
		ev("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", n), "name": call.name, "input": map[string]any{}}})
		args, _ := json.Marshal(call.input)
		half := len(args) / 2
		for _, part := range []string{string(args[:half]), string(args[half:])} {
			ev("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
		}
	} else {
		ev("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		third := len(text) / 3
		for _, d := range []string{text[:third], text[third : 2*third], text[2*third:]} {
			ev("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": d}})
		}
	}
	ev("content_block_stop", map[string]any{"index": 0})
	ev("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 7}})
	ev("message_stop", map[string]any{})
}

func toolNames(ts []anthTool) string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// shellCommand is the command a prompt asks the mock to run: everything
// after "RUN: ", or the first line that starts with "$ ".
func shellCommand(prompt string) string {
	if strings.HasPrefix(prompt, "RUN: ") {
		return strings.TrimPrefix(prompt, "RUN: ")
	}
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "$ ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "$ "))
		}
	}
	return ""
}
