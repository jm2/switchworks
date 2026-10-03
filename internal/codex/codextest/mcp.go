// SPDX-License-Identifier: GPL-3.0-or-later

package codextest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/jsonrpc"
)

// MCP support: like Codex, the fake reads per-thread MCP servers from the
// `config.mcp_servers` of thread/start and thread/resume, starts them with
// only HOME, PATH and the configured env, and calls their tools when a
// prompt says so:
//
//   - "CALL <tool> <json args>" (the rest of that line): calls the tool and
//     replies "tool <tool>: <result text>"
//   - a Switchyard ask ("[switchyard msg=<id> ... kind=ask"): answers it
//     with fleet_reply, text "answer: <last line of the question>"

type mcpConfig struct {
	Name    string
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

func parseMCPConfig(p map[string]any) *mcpConfig {
	cfg, _ := p["config"].(map[string]any)
	servers, _ := cfg["mcp_servers"].(map[string]any)
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, _ := json.Marshal(servers[n])
		var c mcpConfig
		if json.Unmarshal(b, &c) == nil && c.Command != "" {
			c.Name = n
			return &c
		}
	}
	return nil
}

type mcpClient struct {
	cfg  *mcpConfig
	cmd  *exec.Cmd
	conn *jsonrpc.Conn
}

type nopHandler struct{}

func (nopHandler) HandleRequest(json.RawMessage, string, json.RawMessage) {}
func (nopHandler) HandleNotification(string, json.RawMessage)             {}

func startMCP(cfg *mcpConfig) (*mcpClient, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{cfg: cfg, cmd: cmd, conn: jsonrpc.NewConnWith(out, in, nopHandler{}, nil, jsonrpc.Options{VersionField: true})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.conn.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "codex-mcp-client", "version": "fake"}}, &init); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}
	_ = c.conn.Notify("notifications/initialized", nil)
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := c.conn.Call(ctx, "tools/list", map[string]any{}, &tools); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("mcp tools/list: %w", err)
	}
	return c, nil
}

func (c *mcpClient) call(tool string, args json.RawMessage) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := c.conn.Call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &res); err != nil {
		return "", true, err
	}
	var parts []string
	for _, p := range res.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

type mcpState struct {
	mu      sync.Mutex
	configs map[string]*mcpConfig // thread id -> config
	clients map[string]*mcpClient // thread id -> running server
}

func (m *mcpState) setConfig(thread string, cfg *mcpConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configs == nil {
		m.configs = map[string]*mcpConfig{}
		m.clients = map[string]*mcpClient{}
	}
	if cfg == nil {
		delete(m.configs, thread)
		return
	}
	m.configs[thread] = cfg
}

func (m *mcpState) client(thread string) (*mcpClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.clients[thread]; c != nil {
		return c, nil
	}
	cfg := m.configs[thread]
	if cfg == nil {
		return nil, errors.New("no MCP server configured for this thread")
	}
	c, err := startMCP(cfg)
	if err != nil {
		return nil, err
	}
	m.clients[thread] = c
	return c, nil
}

var (
	callRe = regexp.MustCompile(`(?m)^CALL (\S+)\s*(.*)$`)
	askRe  = regexp.MustCompile(`^\[switchyard msg=(\S+) from=(\S+) kind=ask`)
)

// toolCall runs one MCP tool call inside a turn, emitting the mcpToolCall
// item as Codex does, and returns the result text.
func (s *server) toolCall(t *turn, n int, tool string, args json.RawMessage) string {
	itemID := fmt.Sprintf("%s-mcp-%d", t.id, n)
	server := "swyd"
	s.mcp.mu.Lock()
	if cfg := s.mcp.configs[t.thread]; cfg != nil {
		server = cfg.Name
	}
	s.mcp.mu.Unlock()
	item := map[string]any{"type": "mcpToolCall", "id": itemID, "server": server, "tool": tool,
		"arguments": json.RawMessage(args), "status": "inProgress", "result": nil, "error": nil}
	s.note("item/started", map[string]any{"threadId": t.thread, "turnId": t.id, "item": item, "startedAtMs": time.Now().UnixMilli()})
	start := time.Now()
	var text string
	var isErr bool
	c, err := s.mcp.client(t.thread)
	if err == nil {
		text, isErr, err = c.call(tool, args)
	}
	done := map[string]any{}
	for k, v := range item {
		done[k] = v
	}
	done["durationMs"] = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		done["status"], done["error"] = "failed", map[string]any{"message": err.Error()}
		text = "error: " + err.Error()
	default:
		done["status"] = "completed"
		if isErr {
			done["status"] = "failed"
		}
		done["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	}
	s.note("item/completed", map[string]any{"threadId": t.thread, "turnId": t.id, "item": done, "completedAtMs": time.Now().UnixMilli()})
	return text
}

// scriptedTools handles the MCP prompt directives; ok is false when the
// prompt has none.
func (s *server) scriptedTools(t *turn, text string) (reply string, ok bool) {
	if m := askRe.FindStringSubmatch(text); m != nil {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		question := lines[len(lines)-1]
		args, _ := json.Marshal(map[string]string{"message_id": m[1], "text": "answer: " + question})
		res := s.toolCall(t, 1, "fleet_reply", args)
		return "replied to " + m[2] + ": " + res, true
	}
	if m := callRe.FindStringSubmatch(text); m != nil {
		res := s.toolCall(t, 1, m[1], json.RawMessage(strings.TrimSpace(m[2])))
		return "tool " + m[1] + ": " + res, true
	}
	return "", false
}
