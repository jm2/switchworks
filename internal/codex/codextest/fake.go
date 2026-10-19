// SPDX-License-Identifier: GPL-3.0-or-later

// Package codextest provides a scripted stand-in for `codex app-server`,
// speaking the same JSONL JSON-RPC, for deterministic tests of everything
// above the Codex client. Message shapes follow the committed 0.160.0 schema
// and the traffic recorded from a real app-server (see testdata in
// internal/codex).
//
// Tests run it by re-executing the test binary:
//
//	func TestMain(m *testing.M) {
//	    if codextest.MaybeRun() { return }
//	    os.Exit(m.Run())
//	}
//
// and pointing the adapter at os.Args[0] with codextest.Env(stateDir).
//
// Turn behavior is driven by the prompt text:
//   - "APPROVE": requests command approval and reports the decision
//   - "SLOW":    stays in progress until turn/interrupt or turn/steer
//   - "CRASH":   exits the process mid-turn
//   - "CALL <tool> <json>", or a Switchworks ask: calls MCP tools (mcp.go)
//   - otherwise: replies "echo: <prompt>" in three deltas
package codextest

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const envFlag = "SWK_FAKE_APPSERVER"
const envState = "SWK_FAKE_STATE"

// Env returns environment overrides that make a re-executed test binary act
// as the fake app-server, persisting thread ids under stateDir so that
// thread/resume works across fake restarts.
func Env(stateDir string) map[string]string {
	return map[string]string{envFlag: "1", envState: stateDir}
}

// MaybeRun runs the fake app-server if this process was started as one, and
// reports whether it did (the caller should then exit).
func MaybeRun() bool {
	if os.Getenv(envFlag) != "1" {
		return false
	}
	Run(os.Stdin, os.Stdout, os.Getenv(envState))
	os.Exit(0)
	return true
}

type server struct {
	out      *json.Encoder
	outMu    sync.Mutex
	stateDir string

	mu       sync.Mutex
	nextReq  int
	waiting  map[string]chan json.RawMessage // our request id -> response
	threads  map[string]bool
	sandbox  map[string]string // threadId -> sandbox and permissions of its last start or resume
	turns    map[string]*turn  // threadId -> active turn
	turnSeq  int
	initDone bool

	mcp mcpState
}

type turn struct {
	id      string
	thread  string
	stop    chan string // receives "interrupted" or steer text
	images  [][]byte    // image inputs, read when the turn started
	content []any       // the input as sent, echoed in the userMessage item
	policy  string      // the turn's approvalPolicy override, if any
}

// readInput decodes a turn's UserInput list: the text of the first text
// input, and the bytes of every image input. Like app-server, it reads a
// localImage file when the turn starts, so it fails if the file is gone.
func readInput(v any) (string, [][]byte, []any, error) {
	in, _ := v.([]any)
	text, sawText := "", false
	var images [][]byte
	for _, x := range in {
		m, _ := x.(map[string]any)
		switch m["type"] {
		case "text":
			if !sawText {
				text, _ = m["text"].(string)
				sawText = true
			}
		case "localImage":
			p, _ := m["path"].(string)
			b, err := os.ReadFile(p)
			if err != nil {
				return "", nil, nil, fmt.Errorf("localImage %s: %v", p, err)
			}
			images = append(images, b)
		case "image":
			u, _ := m["url"].(string)
			b := DecodeDataURL(u)
			if b == nil {
				return "", nil, nil, fmt.Errorf("image url is not a base64 data URL")
			}
			images = append(images, b)
		}
	}
	if in == nil {
		in = []any{}
	}
	return text, images, in, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Run serves the fake protocol until stdin closes.
func Run(in io.Reader, out io.Writer, stateDir string) {
	s := &server{
		out: json.NewEncoder(out), stateDir: stateDir,
		waiting: map[string]chan json.RawMessage{}, threads: map[string]bool{}, sandbox: map[string]string{}, turns: map[string]*turn{},
	}
	s.loadThreads()
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handle(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *server) send(v any) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_ = s.out.Encode(v)
}

func (s *server) note(method string, params any) {
	s.send(map[string]any{"method": method, "params": params, "emittedAtMs": time.Now().UnixMilli()})
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"id": id, "result": result})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *server) loadThreads() {
	if s.stateDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(s.stateDir, "threads"))
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(b)) {
		s.threads[id] = true
	}
}

func (s *server) saveThread(id string) {
	s.threads[id] = true
	if s.stateDir == "" {
		return
	}
	_ = os.MkdirAll(s.stateDir, 0o755)
	f, err := os.OpenFile(filepath.Join(s.stateDir, "threads"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, id)
		f.Close()
	}
}

func threadObj(id, cwd string, status string) map[string]any {
	return map[string]any{
		"id": id, "sessionId": id, "preview": "", "modelProvider": "fake", "model": "fake-model",
		"cwd": cwd, "ephemeral": false, "createdAt": time.Now().Unix(), "status": map[string]any{"type": status},
		"name": nil, "parentThreadId": nil, "path": nil,
	}
}

func startResp(id, cwd string) map[string]any {
	return map[string]any{
		"thread": threadObj(id, cwd, "idle"), "model": "fake-model", "modelProvider": "fake", "cwd": cwd,
		"approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "workspaceWrite"},
	}
}

func (s *server) handle(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.Method == "" && len(m.ID) > 0 {
		s.mu.Lock()
		ch := s.waiting[string(m.ID)]
		delete(s.waiting, string(m.ID))
		s.mu.Unlock()
		if ch != nil {
			if m.Result != nil {
				ch <- m.Result
			} else {
				ch <- m.Error
			}
		}
		return
	}
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	if m.Method != "initialize" && m.Method != "initialized" && !s.initDone {
		s.fail(m.ID, -32600, "Not initialized")
		return
	}
	// Like app-server, a thread takes a sandbox mode or a permission
	// profile, not both.
	if (m.Method == "thread/start" || m.Method == "thread/resume") && str("sandbox") != "" && str("permissions") != "" {
		s.fail(m.ID, -32600, "`permissions` cannot be combined with `sandbox`")
		return
	}
	switch m.Method {
	case "initialize":
		s.initDone = true
		s.reply(m.ID, map[string]any{"userAgent": "fake/0.0.0", "codexHome": "/fake/codex-home",
			"platformFamily": "unix", "platformOs": "linux"})
	case "initialized":
	case "thread/start":
		id := newID()
		s.mu.Lock()
		s.saveThread(id)
		s.sandbox[id] = "sandbox=" + str("sandbox") + " permissions=" + str("permissions")
		s.mu.Unlock()
		cwd := str("cwd")
		s.mcp.setConfig(id, parseMCPConfig(p))
		// Real app-server sends the response and thread/started back to back;
		// emit the notification first to exercise early-notification buffering.
		s.note("thread/started", map[string]any{"thread": threadObj(id, cwd, "idle")})
		s.reply(m.ID, startResp(id, cwd))
	case "thread/resume":
		id := str("threadId")
		s.mu.Lock()
		ok := s.threads[id]
		if ok {
			s.sandbox[id] = "sandbox=" + str("sandbox") + " permissions=" + str("permissions")
		}
		s.mu.Unlock()
		if !ok {
			s.fail(m.ID, -32600, "thread not found: "+id)
			return
		}
		s.mcp.setConfig(id, parseMCPConfig(p))
		s.reply(m.ID, startResp(id, "/resumed"))
	case "thread/name/set":
		s.reply(m.ID, map[string]any{})
	case "thread/unsubscribe":
		s.reply(m.ID, map[string]any{"status": "unsubscribed"})
	case "turn/start":
		tid := str("threadId")
		text, images, content, err := readInput(p["input"])
		if err != nil {
			s.fail(m.ID, -32600, err.Error())
			return
		}
		s.mu.Lock()
		if s.turns[tid] != nil {
			s.mu.Unlock()
			s.fail(m.ID, -32600, "turn already active")
			return
		}
		s.turnSeq++
		t := &turn{id: fmt.Sprintf("turn-%d", s.turnSeq), thread: tid, stop: make(chan string, 4), images: images, content: content, policy: str("approvalPolicy")}
		s.turns[tid] = t
		s.mu.Unlock()
		s.reply(m.ID, map[string]any{"turn": map[string]any{"id": t.id, "items": []any{}, "status": "inProgress", "error": nil}})
		go s.runTurn(t, text)
	case "turn/steer":
		tid := str("threadId")
		s.mu.Lock()
		t := s.turns[tid]
		s.mu.Unlock()
		if t == nil || t.id != str("expectedTurnId") {
			s.fail(m.ID, -32600, "no matching active turn")
			return
		}
		text, images, _, err := readInput(p["input"])
		if err != nil {
			s.fail(m.ID, -32600, err.Error())
			return
		}
		if r, ok := SeeImages(text, images); ok {
			text += r
		}
		s.reply(m.ID, map[string]any{"turnId": t.id})
		t.stop <- "steer:" + text
	case "turn/interrupt":
		tid := str("threadId")
		s.mu.Lock()
		t := s.turns[tid]
		s.mu.Unlock()
		s.reply(m.ID, map[string]any{})
		if t != nil {
			t.stop <- "interrupted"
		}
	default:
		s.fail(m.ID, -32600, "Invalid request: unknown variant `"+m.Method+"`")
	}
}

func (s *server) request(method string, params any) json.RawMessage {
	s.mu.Lock()
	s.nextReq++
	id := fmt.Sprintf("%d", s.nextReq)
	ch := make(chan json.RawMessage, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	s.send(map[string]any{"id": json.RawMessage(id), "method": method, "params": params})
	return <-ch
}

func (s *server) agentMessage(t *turn, itemID, text string, deltas bool) {
	base := map[string]any{"threadId": t.thread, "turnId": t.id}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	s.note("item/started", with(map[string]any{"item": map[string]any{"type": "agentMessage", "id": itemID, "text": ""},
		"startedAtMs": time.Now().UnixMilli()}))
	if deltas {
		third := len(text) / 3
		for _, part := range []string{text[:third], text[third : 2*third], text[2*third:]} {
			s.note("item/agentMessage/delta", with(map[string]any{"itemId": itemID, "delta": part}))
		}
	}
	s.note("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "id": itemID, "text": text},
		"completedAtMs": time.Now().UnixMilli()}))
}

func (s *server) runTurn(t *turn, text string) {
	defer func() {
		s.mu.Lock()
		delete(s.turns, t.thread)
		s.mu.Unlock()
	}()
	s.note("thread/status/changed", map[string]any{"threadId": t.thread, "status": map[string]any{"type": "active", "activeFlags": []any{}}})
	s.note("turn/started", map[string]any{"threadId": t.thread, "turn": map[string]any{"id": t.id, "items": []any{}, "status": "inProgress", "error": nil}})
	content := t.content
	if content == nil {
		content = []any{map[string]any{"type": "text", "text": text}}
	}
	s.note("item/started", map[string]any{"threadId": t.thread, "turnId": t.id, "startedAtMs": time.Now().UnixMilli(),
		"item": map[string]any{"type": "userMessage", "id": t.id + "-u", "content": content}})
	status := "completed"
	reply := "echo: " + text
	seen, withImages := SeeImages(text, t.images)
	switch {
	case withImages && strings.Contains(strings.ToLower(text), "code in this image"):
		reply = seen
	case strings.Contains(text, "CRASH"):
		os.Exit(3)
	case strings.Contains(text, "POLICY"):
		reply = "policy: " + t.policy
	case strings.Contains(text, "SANDBOX"):
		s.mu.Lock()
		reply = s.sandbox[t.thread]
		s.mu.Unlock()
	case strings.Contains(text, "APPROVE"):
		s.note("thread/status/changed", map[string]any{"threadId": t.thread, "status": map[string]any{"type": "active", "activeFlags": []any{"waitingOnApproval"}}})
		res := s.request("item/commandExecution/requestApproval", map[string]any{
			"threadId": t.thread, "turnId": t.id, "itemId": t.id + "-cmd", "startedAtMs": time.Now().UnixMilli(),
			"command": "make test", "cwd": "/work", "reason": "run the tests",
			// Real app-server 0.160.0 offers this set for a plain command
			// (recorded in internal/codex/testdata): no "decline".
			"availableDecisions": []any{"accept",
				map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []any{"make", "test"}}},
				"cancel"},
		})
		var d struct {
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(res, &d)
		s.note("serverRequest/resolved", map[string]any{"threadId": t.thread, "requestId": json.RawMessage(fmt.Sprintf("%d", s.nextReq))})
		s.note("thread/status/changed", map[string]any{"threadId": t.thread, "status": map[string]any{"type": "active", "activeFlags": []any{}}})
		reply = "approval decision: " + d.Decision
	case strings.Contains(text, "SLOW"):
		select {
		case msg := <-t.stop:
			if msg == "interrupted" {
				status = "interrupted"
				reply = ""
			} else {
				reply = "steered: " + strings.TrimPrefix(msg, "steer:")
			}
		case <-time.After(30 * time.Second):
		}
	default:
		if r, ok := s.scriptedTools(t, text); ok {
			reply = r
		}
	}
	if withImages && reply != "" && reply != seen {
		reply += seen // what it saw, for tests to check
	}
	if reply != "" {
		s.agentMessage(t, t.id+"-msg", reply, true)
	}
	if status == "completed" {
		s.note("turn/diff/updated", map[string]any{"threadId": t.thread, "turnId": t.id, "diff": "--- a/x\n+++ b/x\n"})
	}
	u := map[string]any{"totalTokens": 127, "inputTokens": 120, "cachedInputTokens": 40, "cacheWriteInputTokens": 0, "outputTokens": 7, "reasoningOutputTokens": 0}
	s.note("thread/tokenUsage/updated", map[string]any{"threadId": t.thread, "turnId": t.id,
		"tokenUsage": map[string]any{"total": u, "last": u, "modelContextWindow": 200000}})
	s.note("thread/status/changed", map[string]any{"threadId": t.thread, "status": map[string]any{"type": "idle"}})
	s.note("turn/completed", map[string]any{"threadId": t.thread, "turn": map[string]any{"id": t.id, "items": []any{},
		"status": status, "error": nil, "durationMs": 42}})
}
