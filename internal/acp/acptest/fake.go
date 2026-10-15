// SPDX-License-Identifier: GPL-3.0-or-later

// Package acptest provides a scripted ACP agent for deterministic tests of
// the ACP adapter. Message shapes follow the ACP v1 schema and traffic
// recorded from goose 1.54.0 (string permission request ids, goose _meta,
// usage on the prompt response).
//
// Tests run it by re-executing the test binary:
//
//	func TestMain(m *testing.M) {
//	    if acptest.MaybeRun() { return }
//	    os.Exit(m.Run())
//	}
//
// and pointing the adapter at os.Args[0] with acptest.Env(stateDir).
//
// Turn behavior is driven by the prompt text:
//   - "APPROVE": a shell tool call that asks permission first; reports the
//     outcome ("approved <optionId>", "declined") or stops cancelled
//   - "EDIT":    an edit tool call with a diff
//   - "MCP":     a goose-style MCP tool call (swyd__fleet_list)
//   - "THINK":   a thought chunk, then the reply
//   - "PLAN":    a plan update, then the reply
//   - "SLOW":    waits for session/cancel, then stops "cancelled"
//   - "STUCK":   ignores session/cancel
//   - "CRASH":   exits mid-turn
//   - "REFUSE":  stops with "refusal"
//   - "FAIL":    answers session/prompt with an error
//   - otherwise: replies "echo: <prompt>" in three chunks
//
// Every session's prompts, mode, config and MCP servers are kept in
// <stateDir>/<sessionId>.json, so session/load works across fake restarts
// and tests can inspect what the adapter sent.
package acptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	envFlag  = "SWYD_FAKE_ACP"
	envState = "SWYD_FAKE_ACP_STATE"
	// EnvNoLoad makes the fake advertise neither session/load nor resume.
	EnvNoLoad = "SWYD_FAKE_ACP_NOLOAD"
	// EnvResumeOnly makes the fake offer session/resume instead of load.
	EnvResumeOnly = "SWYD_FAKE_ACP_RESUMEONLY"
	// EnvSlowNew delays session/new by this many milliseconds.
	EnvSlowNew = "SWYD_FAKE_ACP_SLOWNEW"
	// EnvPidFile makes the fake append its pid to this file at start.
	EnvPidFile = "SWYD_FAKE_ACP_PIDFILE"
)

// Env returns environment overrides that make a re-executed test binary act
// as the fake agent with state under stateDir.
func Env(stateDir string) map[string]string {
	return map[string]string{envFlag: "1", envState: stateDir}
}

// MaybeRun runs the fake agent if this process was started as one.
func MaybeRun() bool {
	if os.Getenv(envFlag) != "1" {
		return false
	}
	Run(os.Stdin, os.Stdout, os.Getenv(envState))
	os.Exit(0)
	return true
}

// Session is the persisted state of one fake session.
type Session struct {
	ID         string            `json:"id"`
	Cwd        string            `json:"cwd"`
	Mode       string            `json:"mode"`
	Config     map[string]string `json:"config"`
	McpServers json.RawMessage   `json:"mcp_servers"`
	History    [][2]string       `json:"history"` // prompt, reply
	Loads      int               `json:"loads"`
	Closed     bool              `json:"closed"`
}

// ReadSession loads a session's state for test assertions.
func ReadSession(stateDir, id string) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, id+".json"))
	if err != nil {
		return nil, err
	}
	var s Session
	return &s, json.Unmarshal(b, &s)
}

type agent struct {
	out      io.Writer
	wmu      sync.Mutex
	stateDir string

	mu       sync.Mutex
	sessions map[string]*Session
	cancel   map[string]chan struct{}        // active turn per session
	waiting  map[string]chan json.RawMessage // permission requests by id
	nextReq  int
	usage    int64
}

func (a *agent) send(v any) {
	b, _ := json.Marshal(v)
	a.wmu.Lock()
	defer a.wmu.Unlock()
	fmt.Fprintf(a.out, "%s\n", b)
}

func (a *agent) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *agent) fail(id json.RawMessage, code int, msg string) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (a *agent) update(sid string, u map[string]any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": u}})
	time.Sleep(5 * time.Millisecond)
}

func (a *agent) save(s *Session) {
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(filepath.Join(a.stateDir, s.ID+".json"), b, 0o600)
}

func (a *agent) load(id string) *Session {
	if s := a.sessions[id]; s != nil {
		return s
	}
	s, err := ReadSession(a.stateDir, id)
	if err != nil {
		return nil
	}
	a.sessions[id] = s
	return s
}

// Run serves the fake agent on in/out.
func Run(in io.Reader, out io.Writer, stateDir string) {
	if stateDir == "" {
		stateDir = os.TempDir()
	}
	_ = os.MkdirAll(stateDir, 0o700)
	if pf := os.Getenv(EnvPidFile); pf != "" {
		if f, err := os.OpenFile(pf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
		}
	}
	a := &agent{out: out, stateDir: stateDir, sessions: map[string]*Session{},
		cancel: map[string]chan struct{}{}, waiting: map[string]chan json.RawMessage{}}
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			a.handle(line)
		}
		if err != nil {
			return
		}
	}
}

func configOptions(s *Session) []any {
	sel := func(id string, values ...string) map[string]any {
		opts := make([]any, 0, len(values))
		for _, v := range values {
			opts = append(opts, map[string]any{"value": v, "name": v})
		}
		return map[string]any{"id": id, "name": id, "type": "select", "currentValue": s.Config[id], "options": opts}
	}
	return []any{
		sel("provider", "goose", "anthropic", "deepseek"),
		sel("mode", "auto", "approve", "smart_approve", "chat"),
		sel("model", "mock-small", "mock-large"),
	}
}

func setup(s *Session) map[string]any {
	return map[string]any{
		"modes": map[string]any{"currentModeId": s.Config["mode"], "availableModes": []any{
			map[string]any{"id": "auto", "name": "auto"}, map[string]any{"id": "approve", "name": "approve"},
			map[string]any{"id": "smart_approve", "name": "smart_approve"}, map[string]any{"id": "chat", "name": "chat"},
		}},
		"configOptions": configOptions(s),
	}
}

func (a *agent) handle(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.Method == "" { // a response to our permission request
		var id string
		_ = json.Unmarshal(m.ID, &id)
		a.mu.Lock()
		ch := a.waiting[id]
		delete(a.waiting, id)
		a.mu.Unlock()
		if ch != nil {
			ch <- m.Result
		}
		return
	}
	var p struct {
		SessionID  string          `json:"sessionId"`
		Cwd        string          `json:"cwd"`
		McpServers json.RawMessage `json:"mcpServers"`
		ModeID     string          `json:"modeId"`
		ConfigID   string          `json:"configId"`
		Value      string          `json:"value"`
		Prompt     []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch m.Method {
	case "initialize":
		caps := map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"close": map[string]any{}},
			"mcpCapabilities": map[string]any{"http": false, "sse": false}}
		if os.Getenv(EnvNoLoad) == "1" {
			caps = map[string]any{"loadSession": false}
		}
		if os.Getenv(EnvResumeOnly) == "1" {
			caps = map[string]any{"loadSession": false, "sessionCapabilities": map[string]any{"resume": map[string]any{}}}
		}
		a.reply(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": caps,
			"authMethods": []any{}, "agentInfo": map[string]any{"name": "fake-acp", "version": "0.1"}})
	case "session/new":
		if ms, _ := strconv.Atoi(os.Getenv(EnvSlowNew)); ms > 0 {
			a.mu.Unlock()
			time.Sleep(time.Duration(ms) * time.Millisecond)
			a.mu.Lock()
		}
		id := fmt.Sprintf("fake_%d", time.Now().UnixNano())
		s := &Session{ID: id, Cwd: p.Cwd, McpServers: p.McpServers,
			Config: map[string]string{"mode": "smart_approve", "model": "mock-small", "provider": "goose"}}
		a.sessions[id] = s
		a.save(s)
		r := setup(s)
		r["sessionId"] = id
		a.reply(m.ID, r)
	case "session/load", "session/resume":
		s := a.load(p.SessionID)
		if s == nil {
			a.fail(m.ID, -32002, "session not found: "+p.SessionID)
			return
		}
		s.Loads++
		s.McpServers = p.McpServers
		a.save(s)
		if m.Method == "session/load" {
			for _, turn := range s.History {
				a.update(s.ID, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": turn[0]}})
				a.update(s.ID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": turn[1]}})
			}
		}
		a.reply(m.ID, setup(s))
	case "session/set_mode":
		if s := a.load(p.SessionID); s != nil {
			s.Config["mode"] = p.ModeID
			s.Mode = p.ModeID
			a.save(s)
		}
		a.reply(m.ID, map[string]any{})
	case "session/set_config_option":
		s := a.load(p.SessionID)
		if s == nil {
			a.fail(m.ID, -32002, "no session")
			return
		}
		valid := false
		for _, o := range configOptions(s) {
			om := o.(map[string]any)
			if om["id"] != p.ConfigID {
				continue
			}
			for _, v := range om["options"].([]any) {
				if v.(map[string]any)["value"] == p.Value {
					valid = true
				}
			}
		}
		if !valid {
			a.fail(m.ID, -32602, fmt.Sprintf("invalid value %q for %s", p.Value, p.ConfigID))
			return
		}
		s.Config[p.ConfigID] = p.Value
		if p.ConfigID == "mode" {
			s.Mode = p.Value
		}
		a.save(s)
		a.reply(m.ID, map[string]any{"configOptions": configOptions(s)})
	case "session/close":
		if s := a.load(p.SessionID); s != nil {
			s.Closed = true
			a.save(s)
		}
		a.reply(m.ID, map[string]any{})
	case "session/cancel":
		if ch := a.cancel[p.SessionID]; ch != nil {
			close(ch)
			delete(a.cancel, p.SessionID)
		}
	case "session/prompt":
		s := a.load(p.SessionID)
		if s == nil {
			a.fail(m.ID, -32002, "no session")
			return
		}
		text := ""
		for _, b := range p.Prompt {
			if b.Type == "text" {
				text += b.Text
			}
		}
		cancel := make(chan struct{})
		a.cancel[s.ID] = cancel
		go a.turn(m.ID, s, text, cancel)
	default:
		if len(m.ID) > 0 {
			a.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

// ask sends a permission request and waits for the answer or a cancel.
func (a *agent) ask(sid string, toolCall map[string]any, cancel chan struct{}) (outcome, optionID string) {
	a.mu.Lock()
	a.nextReq++
	id := fmt.Sprintf("perm-%d-%d", os.Getpid(), a.nextReq)
	ch := make(chan json.RawMessage, 1)
	a.waiting[id] = ch
	a.mu.Unlock()
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": map[string]any{
		"sessionId": sid, "toolCall": toolCall, "options": []any{
			map[string]any{"optionId": "allow_always", "name": "Always", "kind": "allow_always"},
			map[string]any{"optionId": "allow_once", "name": "Once", "kind": "allow_once"},
			map[string]any{"optionId": "reject_once", "name": "No", "kind": "reject_once"},
			map[string]any{"optionId": "reject_always", "name": "Never", "kind": "reject_always"},
		}}})
	select {
	case r := <-ch:
		var resp struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(r, &resp)
		return resp.Outcome.Outcome, resp.Outcome.OptionID
	case <-cancel:
		// The client must still answer "cancelled"; wait briefly for it.
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
		return "cancelled", ""
	}
}

func (a *agent) turn(id json.RawMessage, s *Session, text string, cancel chan struct{}) {
	sid := s.ID
	msg := func(t string) {
		third := len(t) / 3
		for _, part := range []string{t[:third], t[third : 2*third], t[2*third:]} {
			a.update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m-" + fmt.Sprint(len(t)),
				"content": map[string]any{"type": "text", "text": part}})
		}
	}
	finish := func(stop, replyText string) {
		a.mu.Lock()
		if a.cancel[sid] == cancel {
			delete(a.cancel, sid)
		}
		if stop == "end_turn" {
			s.History = append(s.History, [2]string{text, replyText})
			a.save(s)
		}
		a.usage += 100
		u := a.usage
		a.mu.Unlock()
		a.reply(id, map[string]any{"stopReason": stop, "usage": map[string]any{
			"totalTokens": u, "inputTokens": u - u/10, "outputTokens": u / 10, "cachedReadTokens": u / 4}})
	}
	a.update(sid, map[string]any{"sessionUpdate": "usage_update", "used": 10, "size": 128000})
	switch {
	case strings.Contains(text, "FAIL"):
		a.mu.Lock()
		delete(a.cancel, sid)
		a.mu.Unlock()
		a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "provider error", "data": "no API key"}})
	case strings.Contains(text, "CRASH"):
		msg("about to crash")
		os.Exit(3)
	case strings.Contains(text, "REFUSE"):
		finish("refusal", "")
	case strings.Contains(text, "SLOW"):
		msg("working")
		<-cancel
		finish("cancelled", "")
	case strings.Contains(text, "STUCK"):
		msg("stuck")
		<-cancel
		select {} // ignore the cancel forever
	case strings.Contains(text, "APPROVE"):
		tc := map[string]any{"toolCallId": "tc-run", "title": "shell · touch x", "kind": "execute", "status": "pending",
			"rawInput": map[string]any{"command": "touch x"}}
		a.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-run", "title": "shell · touch x",
			"kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "touch x"}})
		outcome, opt := a.ask(sid, tc, cancel)
		switch {
		case outcome == "cancelled":
			a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-run", "status": "failed"})
			finish("cancelled", "")
		case strings.HasPrefix(opt, "allow"):
			a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-run", "status": "in_progress"})
			a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-run", "status": "completed",
				"content":   []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "ran"}}},
				"rawOutput": map[string]any{"stdout": "ran", "stderr": "", "exit_code": 0}})
			msg("approved " + opt)
			finish("end_turn", "approved "+opt)
		default:
			a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-run", "status": "failed",
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "denied"}}}})
			msg("declined")
			finish("end_turn", "declined")
		}
	case strings.Contains(text, "EDIT"):
		a.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-edit", "title": "edit main.go",
			"kind": "edit", "status": "in_progress", "locations": []any{map[string]any{"path": "/w/main.go"}}})
		a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-edit", "status": "completed",
			"content": []any{map[string]any{"type": "diff", "path": "/w/main.go",
				"oldText": "package main\n\nfunc main() {\n\tprintln(\"a\")\n}\n",
				"newText": "package main\n\nfunc main() {\n\tprintln(\"b\")\n}\n"}}})
		msg("edited")
		finish("end_turn", "edited")
	case strings.Contains(text, "FLEETPERM"):
		meta := map[string]any{"goose": map[string]any{"toolCall": map[string]any{"toolName": "swyd__fleet_reply", "extensionName": "swyd"}}}
		a.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-fp", "title": "swyd: fleet reply",
			"rawInput": map[string]any{}, "_meta": meta})
		outcome, opt := a.ask(sid, map[string]any{"toolCallId": "tc-fp", "title": "swyd: fleet reply", "_meta": meta}, cancel)
		if outcome != "selected" || !strings.HasPrefix(opt, "allow") {
			finish("cancelled", "")
			return
		}
		a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-fp", "status": "completed"})
		msg("fleet tool " + opt)
		finish("end_turn", "fleet tool "+opt)
	case strings.Contains(text, "MCP"):
		a.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-mcp", "title": "swyd: fleet list",
			"rawInput": map[string]any{}, "_meta": map[string]any{"goose": map[string]any{
				"toolCall": map[string]any{"toolName": "swyd__fleet_list", "extensionName": "swyd"}}}})
		a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-mcp", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "2 sessions"}}}})
		msg("listed")
		finish("end_turn", "listed")
	case strings.Contains(text, "THINK"):
		a.update(sid, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "hmm"}})
		msg("thought")
		finish("end_turn", "thought")
	case strings.Contains(text, "PLAN"):
		a.update(sid, map[string]any{"sessionUpdate": "plan", "entries": []any{
			map[string]any{"content": "step one", "priority": "high", "status": "pending"}}})
		msg("planned")
		finish("end_turn", "planned")
	default:
		r := "echo: " + text
		msg(r)
		finish("end_turn", r)
	}
}
