// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/jsonrpc"
)

type nop struct{}

func (nop) HandleRequest(json.RawMessage, string, json.RawMessage) {}
func (nop) HandleNotification(string, json.RawMessage)             {}

// stubNode is a fake node local API.
type stubNode struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string][]json.RawMessage
	fail   int // answer this many calls with a transient error first
	ask    func(args fleet.AskArgs) fleet.AskResult
	inbox  func(args fleet.InboxArgs) fleet.InboxResult
	// slow, if set, holds each inbox call this long (or until the client
	// gives up) before answering.
	slow func(args fleet.InboxArgs) time.Duration
}

func (s *stubNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/fleet/")
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls = append(s.calls, op)
	if s.bodies == nil {
		s.bodies = map[string][]json.RawMessage{}
	}
	s.bodies[op] = append(s.bodies[op], body)
	fail := s.fail > 0
	if fail {
		s.fail--
	}
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Swk-Session") != "sess1" {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad credential"})
		return
	}
	if fail {
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "reconnecting", "transient": true})
		return
	}
	var data any
	switch op {
	case "list":
		data = fleet.ListResult{Self: "devbox/a", Sessions: []fleet.SessionInfo{{Path: "buildbox/b", Status: "idle"}}}
	case "ask":
		var a fleet.AskArgs
		_ = json.Unmarshal(body, &a)
		data = s.ask(a)
	case "inbox":
		var a fleet.InboxArgs
		_ = json.Unmarshal(body, &a)
		if s.slow != nil {
			select {
			case <-time.After(s.slow(a)):
			case <-r.Context().Done():
				return
			}
		}
		data = s.inbox(a)
	case "send":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "rate limit: too many messages"})
		return
	default:
		data = map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

func startServer(t *testing.T, node *stubNode) *jsonrpc.Conn {
	t.Helper()
	hs := httptest.NewServer(node)
	t.Cleanup(hs.Close)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		_ = Serve(sr, sw, &NodeClient{URL: hs.URL, Session: "sess1", Token: "tok"}, t.Logf)
		sw.Close()
	}()
	t.Cleanup(func() { cw.Close() })
	return jsonrpc.NewConnWith(cr, cw, nop{}, nil, jsonrpc.Options{VersionField: true})
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func callTool(t *testing.T, c *jsonrpc.Conn, name string, args any) toolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res toolResult
	if err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestProtocol(t *testing.T) {
	node := &stubNode{fail: 1}
	c := startServer(t, node)
	ctx := context.Background()
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"}}, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "swk" || !strings.Contains(init.Instructions, "fleet_reply") {
		t.Fatalf("initialize: %+v", init)
	}
	_ = c.Notify("notifications/initialized", nil)
	var unknown struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = c.Call(ctx, "initialize", map[string]any{"protocolVersion": "1999-01-01"}, &unknown)
	if unknown.ProtocolVersion != "2025-06-18" {
		t.Fatalf("fallback version %q", unknown.ProtocolVersion)
	}

	var tl struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.Call(ctx, "tools/list", map[string]any{}, &tl); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tl.Tools {
		names[tool.Name] = true
		if tool.InputSchema["type"] != "object" || tool.Description == "" {
			t.Errorf("tool %s: bad definition", tool.Name)
		}
	}
	for _, n := range []string{"fleet_list", "fleet_send", "fleet_ask", "fleet_reply", "fleet_inbox", "fleet_spawn", "fleet_status", "fleet_stop", "fleet_remind", "fleet_lock", "fleet_unlock", "fleet_locks", "workspace_git"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}

	// A transient failure is retried.
	res := callTool(t, c, "fleet_list", map[string]any{})
	if res.IsError || !strings.Contains(res.Content[0].Text, "buildbox/b") {
		t.Fatalf("fleet_list: %+v", res)
	}
	// Errors from the coordinator are tool errors, not protocol errors.
	res = callTool(t, c, "fleet_send", map[string]any{"to": "b", "text": "x"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "rate limit") {
		t.Fatalf("fleet_send: %+v", res)
	}
	// workspace_git goes to the node's own git op.
	res = callTool(t, c, "workspace_git", map[string]any{"op": "status"})
	node.mu.Lock()
	gitBodies := node.bodies["git"]
	node.mu.Unlock()
	if res.IsError || len(gitBodies) != 1 || !strings.Contains(string(gitBodies[0]), `"op":"status"`) {
		t.Fatalf("workspace_git: %+v %s", res, gitBodies)
	}
	if !strings.Contains(init.Instructions, "workspace_git") {
		t.Fatal("instructions do not mention workspace_git")
	}
	res = callTool(t, c, "fleet_nope", map[string]any{})
	if !res.IsError {
		t.Fatal("unknown tool did not fail")
	}
	res = callTool(t, c, "fleet_send", map[string]any{"to": 5})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "bad arguments") {
		t.Fatalf("bad args: %+v", res)
	}
	var e *jsonrpc.Error
	if err := c.Call(ctx, "resources/list", map[string]any{}, nil); err == nil {
		t.Fatal("unsupported method succeeded")
	} else if !asErr(err, &e) || e.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("unsupported method: %v", err)
	}
}

func asErr(err error, target **jsonrpc.Error) bool {
	e, ok := err.(*jsonrpc.Error)
	if ok {
		*target = e
	}
	return ok
}

// fleet_ask sends with wait 0 and a hold, then waits on the ticket in a
// separate call that survives transient failures.
func TestAskTwoPhase(t *testing.T) {
	var mu sync.Mutex
	inboxCalls := 0
	node := &stubNode{
		ask: func(a fleet.AskArgs) fleet.AskResult {
			if a.WaitSeconds == nil || *a.WaitSeconds != 0 || a.HoldSeconds < 60 {
				return fleet.AskResult{Status: fleet.AskFailed, Error: "shim did not use the two-phase ask"}
			}
			return fleet.AskResult{Status: fleet.AskPending, MessageID: "ask1", Target: "buildbox/b"}
		},
		inbox: func(a fleet.InboxArgs) fleet.InboxResult {
			mu.Lock()
			inboxCalls++
			n := inboxCalls
			mu.Unlock()
			if a.Ticket != "ask1" || a.WaitSeconds <= 0 {
				return fleet.InboxResult{Ticket: &fleet.AskResult{Status: fleet.AskFailed, Error: "bad ticket wait"}}
			}
			if n == 1 {
				return fleet.InboxResult{Ticket: &fleet.AskResult{Status: fleet.AskPending, MessageID: "ask1"}}
			}
			return fleet.InboxResult{Ticket: &fleet.AskResult{Status: fleet.AskReplied, MessageID: "ask1", Reply: "42", ReplyFrom: "buildbox/b"}}
		},
	}
	c := startServer(t, node)
	res := callTool(t, c, "fleet_ask", map[string]any{"to": "b", "text": "6x7?", "wait_seconds": 30})
	if res.IsError || res.Content[0].Text != "Answer from buildbox/b:\n42" {
		t.Fatalf("fleet_ask: %+v", res)
	}
	// wait_seconds 0 returns the ticket at once, without waiting.
	node.ask = func(a fleet.AskArgs) fleet.AskResult {
		return fleet.AskResult{Status: fleet.AskPending, MessageID: "ask2", Target: "buildbox/b"}
	}
	res = callTool(t, c, "fleet_ask", map[string]any{"to": "b", "text": "later", "wait_seconds": 0})
	if res.IsError || !strings.Contains(res.Content[0].Text, `"message_id": "ask2"`) || !strings.Contains(res.Content[0].Text, "fleet_inbox") {
		t.Fatalf("ticket: %+v", res)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.bodies["inbox"]) != 2 {
		t.Fatalf("inbox calls: %d", len(node.bodies["inbox"]))
	}
}

// fleet_remind is listed, the instructions say to wait with it rather
// than poll, and a call goes to the node's remind op as given.
func TestRemindTool(t *testing.T) {
	node := &stubNode{}
	c := startServer(t, node)
	ctx := context.Background()
	var init struct {
		Instructions string `json:"instructions"`
	}
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18"}, &init); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fleet_remind", "end your turn", "wait_seconds 0", "acknowledgements quiet"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	var tl struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.Call(ctx, "tools/list", map[string]any{}, &tl); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tl.Tools {
		if tool.Name == "fleet_remind" {
			found = true
			props := tool.InputSchema["properties"].(map[string]any)
			for _, p := range []string{"in_seconds", "text", "id", "cancel"} {
				if props[p] == nil {
					t.Errorf("fleet_remind has no %s", p)
				}
			}
		}
	}
	if !found {
		t.Fatal("fleet_remind not listed")
	}
	if res := callTool(t, c, "fleet_remind", map[string]any{"in_seconds": 600, "text": "check CI on #123", "id": "ci:123"}); res.IsError {
		t.Fatalf("fleet_remind: %+v", res)
	}
	if res := callTool(t, c, "fleet_remind", map[string]any{"cancel": "ci:123"}); res.IsError {
		t.Fatalf("fleet_remind cancel: %+v", res)
	}
	if res := callTool(t, c, "fleet_remind", map[string]any{"in_seconds": "soon"}); !res.IsError {
		t.Fatalf("bad arguments accepted: %+v", res)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	bodies := node.bodies["remind"]
	if len(bodies) != 2 {
		t.Fatalf("remind calls %d", len(bodies))
	}
	var a fleet.RemindArgs
	if err := json.Unmarshal(bodies[0], &a); err != nil || a != (fleet.RemindArgs{InSeconds: 600, Text: "check CI on #123", ID: "ci:123"}) {
		t.Fatalf("forwarded %s (%v)", bodies[0], err)
	}
	var c2 fleet.RemindArgs
	if err := json.Unmarshal(bodies[1], &c2); err != nil || c2 != (fleet.RemindArgs{Cancel: "ci:123"}) {
		t.Fatalf("forwarded %s (%v)", bodies[1], err)
	}
}
