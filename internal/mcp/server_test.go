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

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/jsonrpc"
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
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Swyd-Session") != "sess1" {
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
		data = fleet.ListResult{Self: "antec/a", Sessions: []fleet.SessionInfo{{Path: "boxy/b", Status: "idle"}}}
	case "ask":
		var a fleet.AskArgs
		_ = json.Unmarshal(body, &a)
		data = s.ask(a)
	case "inbox":
		var a fleet.InboxArgs
		_ = json.Unmarshal(body, &a)
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
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "swyd" || !strings.Contains(init.Instructions, "fleet_reply") {
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
	for _, n := range []string{"fleet_list", "fleet_send", "fleet_ask", "fleet_reply", "fleet_inbox", "fleet_spawn", "fleet_status", "fleet_stop"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}

	// A transient failure is retried.
	res := callTool(t, c, "fleet_list", map[string]any{})
	if res.IsError || !strings.Contains(res.Content[0].Text, "boxy/b") {
		t.Fatalf("fleet_list: %+v", res)
	}
	// Errors from the coordinator are tool errors, not protocol errors.
	res = callTool(t, c, "fleet_send", map[string]any{"to": "b", "text": "x"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "rate limit") {
		t.Fatalf("fleet_send: %+v", res)
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
			return fleet.AskResult{Status: fleet.AskPending, MessageID: "ask1", Target: "boxy/b"}
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
			return fleet.InboxResult{Ticket: &fleet.AskResult{Status: fleet.AskReplied, MessageID: "ask1", Reply: "42", ReplyFrom: "boxy/b"}}
		},
	}
	c := startServer(t, node)
	res := callTool(t, c, "fleet_ask", map[string]any{"to": "b", "text": "6x7?", "wait_seconds": 30})
	if res.IsError || res.Content[0].Text != "Answer from boxy/b:\n42" {
		t.Fatalf("fleet_ask: %+v", res)
	}
	// wait_seconds 0 returns the ticket at once, without waiting.
	node.ask = func(a fleet.AskArgs) fleet.AskResult {
		return fleet.AskResult{Status: fleet.AskPending, MessageID: "ask2", Target: "boxy/b"}
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
