// SPDX-License-Identifier: GPL-3.0-or-later

// Package mcp is `swyd mcp`: a stdio MCP server that gives an agent session
// its fleet tools (fleet_list, fleet_send, fleet_ask, fleet_reply,
// fleet_inbox, fleet_spawn, fleet_status, fleet_stop). The harness starts it
// inside the session with SWYD_NODE_URL, SWYD_SESSION and SWYD_TOKEN set by
// the node; every call is forwarded to the node's local API and from there
// to the coordinator.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/jsonrpc"
	"github.com/jm2/switchyard/internal/version"
)

// Protocol versions this server speaks, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Environment variables (set by the node; duplicated here to avoid an
// import cycle).
const (
	envNodeURL     = "SWYD_NODE_URL"
	envSession     = "SWYD_SESSION"
	envSessionPath = "SWYD_SESSION_PATH"
	envToken       = "SWYD_TOKEN"
)

// NodeClient calls the node's local API for one session.
type NodeClient struct {
	URL     string
	Session string
	Token   string
	HTTP    *http.Client
}

type localResponse struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Transient bool            `json:"transient,omitempty"`
}

// errTransient marks failures worth retrying (the node is reconnecting).
type errTransient struct{ msg string }

func (e *errTransient) Error() string { return e.msg }

// Do performs one fleet operation ("list", "send", ...).
func (c *NodeClient) Do(ctx context.Context, op string, args any) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/v1/fleet/"+op, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Swyd-Session", c.Session)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &errTransient{msg: "cannot reach the swyd node: " + err.Error()}
	}
	defer resp.Body.Close()
	var lr localResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&lr); err != nil {
		return nil, fmt.Errorf("bad response from the swyd node (HTTP %d): %w", resp.StatusCode, err)
	}
	if lr.Transient {
		return nil, &errTransient{msg: lr.Error}
	}
	if !lr.OK {
		return nil, errors.New(lr.Error)
	}
	return lr.Data, nil
}

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema map[string]any  `json:"inputSchema"`
	Annotations map[string]bool `json:"annotations,omitempty"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

const addressHelp = `Addresses: "host/name" (a session path), a fleet-unique session name, "parent" (the session that spawned you), "@key=value" (sessions with that label; host and harness also work as keys), or "user:<name>" (a person, who reads it in the Switchyard UI or CLI).`

// Tools are the fleet tools.
var Tools = []Tool{
	{Name: "fleet_list", Title: "List fleet sessions", Annotations: map[string]bool{"readOnlyHint": true},
		Description: "List the agent sessions in the Switchyard fleet, across all hosts, with their status. Use a session's path (host/name) to address it in other fleet tools.",
		InputSchema: obj(map[string]any{
			"host":  str("Only sessions on this host."),
			"label": str(`Only sessions with this label, as "key=value".`),
			"all":   map[string]any{"type": "boolean", "description": "Include stopped sessions."},
		})},
	{Name: "fleet_send", Title: "Send a message",
		Description: "Send a message to another agent session (or a person) without waiting for an answer. " + addressHelp +
			` mode: "queue" (default: delivered as a new turn when the target is idle), "steer" (added to the target's running turn), "interrupt" (cancels the target's running turn and delivers now). Set broadcast to send to every session matching an @selector.`,
		InputSchema: obj(map[string]any{
			"to":        str("Recipient address."),
			"text":      str("The message."),
			"mode":      map[string]any{"type": "string", "enum": []string{"queue", "steer", "interrupt"}, "description": "Delivery mode (default queue)."},
			"broadcast": map[string]any{"type": "boolean", "description": "Send to every session matching an @selector."},
		}, "to", "text")},
	{Name: "fleet_ask", Title: "Ask and wait for an answer",
		Description: "Ask another agent session (or a person) a question and wait for the answer, for up to wait_seconds (default 600, max 1800). " + addressHelp +
			" The recipient answers with fleet_reply. If no answer arrives in time you get a ticket instead: the answer will arrive later as a message in this session, or check it with fleet_inbox(ticket).",
		InputSchema: obj(map[string]any{
			"to":           str("Recipient address."),
			"text":         str("The question."),
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 1800, "description": "How long to wait for the answer (default 600; 0 returns a ticket at once)."},
		}, "to", "text")},
	{Name: "fleet_reply", Title: "Answer a question",
		Description: `Answer a question another session asked you. A question arrives as a message starting with "[switchyard msg=<id> ... kind=ask"; pass that id as message_id.`,
		InputSchema: obj(map[string]any{
			"message_id": str("The id of the ask you are answering."),
			"text":       str("Your answer."),
		}, "message_id", "text")},
	{Name: "fleet_inbox", Title: "Recent messages", Annotations: map[string]bool{"readOnlyHint": true},
		Description: "Show recent messages sent to or from this session. With ticket (an ask id returned by fleet_ask), report whether that ask has been answered, waiting up to wait_seconds for it.",
		InputSchema: obj(map[string]any{
			"ticket":       str("An ask id to check."),
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 1800, "description": "With ticket: how long to wait for the answer."},
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "How many messages (default 20)."},
		})},
	{Name: "fleet_spawn", Title: "Start a new agent session",
		Description: "Start a new agent session on a host to work on a task in parallel. It runs on its own; when it has finished all its work you receive a completion message containing its last reply. You can message it, check it with fleet_status, or stop it with fleet_stop.",
		InputSchema: obj(map[string]any{
			"host":    str("Host to run on (see fleet_list)."),
			"cwd":     str("Working directory on that host."),
			"prompt":  str("The task."),
			"name":    str("Session name (default: derived from yours)."),
			"harness": map[string]any{"type": "string", "description": "Agent harness: codex (default), agy, goose, or another ACP agent the host offers (fleet_list shows each host's harnesses)."},
			"model":   str("Model override."),
			"labels":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Labels for @selectors."},
		}, "host", "cwd", "prompt")},
	{Name: "fleet_status", Title: "Session status", Annotations: map[string]bool{"readOnlyHint": true},
		Description: `Get a session's status, model, token usage and its last message. session: a path, a name, "parent", or omit it for yourself.`,
		InputSchema: obj(map[string]any{"session": str("The session (default: yourself).")})},
	{Name: "fleet_stop", Title: "Stop a session", Annotations: map[string]bool{"destructiveHint": true},
		Description: "Stop a session you spawned (its history is kept).",
		InputSchema: obj(map[string]any{"session": str("The session to stop.")}, "session")},
}

const instructions = `You are one agent session in a Switchyard fleet: other agents run in sessions on several machines. Use the fleet_* tools to see them (fleet_list), message them (fleet_send), ask them questions and wait for answers (fleet_ask), and start helpers (fleet_spawn). Messages from other sessions arrive as turns that start with "[switchyard msg=... from=... kind=...]". When kind=ask, answer with fleet_reply using that msg id.`

// Server serves MCP on one connection.
type Server struct {
	node  *NodeClient
	conn  *jsonrpc.Conn
	ready chan struct{} // closed once conn is set
	logf  func(string, ...any)

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// Serve runs the server on r/w until r closes.
func Serve(r io.Reader, w io.Writer, node *NodeClient, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{node: node, logf: logf, cancels: map[string]context.CancelFunc{}, ready: make(chan struct{})}
	s.conn = jsonrpc.NewConnWith(r, w, s, nil, jsonrpc.Options{VersionField: true})
	close(s.ready)
	<-s.conn.Done()
	s.mu.Lock()
	for _, c := range s.cancels {
		c()
	}
	s.mu.Unlock()
	if err := s.conn.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Main implements `swyd mcp`.
func Main(stdin io.Reader, stdout, stderr io.Writer) int {
	node := &NodeClient{URL: os.Getenv(envNodeURL), Session: os.Getenv(envSession), Token: os.Getenv(envToken)}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, "swyd mcp: "+f+"\n", a...) }
	if node.URL == "" || node.Session == "" || node.Token == "" {
		logf("missing %s/%s/%s: swyd mcp is started by a swyd node inside a managed session", envNodeURL, envSession, envToken)
	}
	if err := Serve(stdin, stdout, node, logf); err != nil {
		logf("%v", err)
		return 1
	}
	return 0
}

func (s *Server) HandleNotification(method string, params json.RawMessage) {
	if method == "notifications/cancelled" {
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.mu.Lock()
			if c := s.cancels[string(p.RequestID)]; c != nil {
				c()
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	<-s.ready
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		ver := protocolVersions[1] // 2025-06-18 unless the client asks for one we know
		for _, v := range protocolVersions {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		_ = s.conn.Reply(id, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": version.Bin, "title": version.Name + " fleet tools", "version": version.Version},
			"instructions":    instructions,
		})
	case "ping":
		_ = s.conn.Reply(id, map[string]any{})
	case "tools/list":
		_ = s.conn.Reply(id, map[string]any{"tools": Tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			_ = s.conn.ReplyError(id, jsonrpc.CodeInvalidParams, err.Error())
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		key := string(id)
		s.mu.Lock()
		s.cancels[key] = cancel
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.cancels, key)
				s.mu.Unlock()
				cancel()
			}()
			// Only text content: Codex 0.160 shows the model a tool's
			// structuredContent instead of its text when both are present,
			// and the text is written for the model.
			text, _, err := s.call(ctx, p.Name, p.Arguments)
			res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": err != nil}
			if err != nil {
				res["content"] = []map[string]any{{"type": "text", "text": "Error: " + err.Error()}}
			}
			_ = s.conn.Reply(id, res)
		}()
	default:
		_ = s.conn.ReplyError(id, jsonrpc.CodeMethodNotFound, "method not supported: "+method)
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("bad arguments: %w", err)
	}
	return v, nil
}

func pretty(raw json.RawMessage) (string, any) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw), nil
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b), v
}

// do calls the node, retrying transient failures until ctx's deadline.
func (s *Server) do(ctx context.Context, op string, args any) (json.RawMessage, error) {
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		data, err := s.node.Do(ctx, op, args)
		var tr *errTransient
		if err == nil || !errors.As(err, &tr) || attempt >= 5 {
			return data, err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff *= 2
	}
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (string, any, error) {
	op := ""
	var args any
	switch name {
	case "fleet_list":
		a, err := decode[fleet.ListArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "list", a
	case "fleet_send":
		a, err := decode[fleet.SendArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "send", a
	case "fleet_ask":
		a, err := decode[fleet.AskArgs](raw)
		if err != nil {
			return "", nil, err
		}
		return s.ask(ctx, a)
	case "fleet_reply":
		a, err := decode[fleet.ReplyArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "reply", a
	case "fleet_inbox":
		a, err := decode[fleet.InboxArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "inbox", a
	case "fleet_spawn":
		a, err := decode[fleet.SpawnArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "spawn", a
	case "fleet_status":
		a, err := decode[fleet.StatusArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "status", a
	case "fleet_stop":
		a, err := decode[fleet.StopArgs](raw)
		if err != nil {
			return "", nil, err
		}
		op, args = "stop", a
	default:
		return "", nil, fmt.Errorf("unknown tool %q", name)
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	data, err := s.do(cctx, op, args)
	if err != nil {
		return "", nil, err
	}
	text, structured := pretty(data)
	return text, structured, nil
}

// ask sends the question at once (getting a ticket), then waits for the
// answer in a separate, retried call, so a coordinator reconnect while
// waiting does not lose the ticket.
func (s *Server) ask(ctx context.Context, a fleet.AskArgs) (string, any, error) {
	wait := fleet.DefaultAskWait
	if a.WaitSeconds != nil {
		wait = time.Duration(*a.WaitSeconds) * time.Second
	}
	if wait > fleet.MaxAskWait {
		wait = fleet.MaxAskWait
	}
	zero := 0
	a.WaitSeconds = &zero
	if wait > 0 {
		// Until then a reply is held for our fleet_inbox wait below rather
		// than also arriving as a new turn.
		a.HoldSeconds = int((wait + time.Minute).Seconds())
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	data, err := s.do(sctx, "ask", a)
	cancel()
	if err != nil {
		return "", nil, err
	}
	var res fleet.AskResult
	if err := json.Unmarshal(data, &res); err != nil {
		return "", nil, err
	}
	deadline := time.Now().Add(wait)
	for res.Status == fleet.AskPending && time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wctx, cancel := context.WithTimeout(ctx, remaining+time.Minute)
		data, err := s.node.Do(wctx, "inbox", fleet.InboxArgs{Ticket: res.MessageID, WaitSeconds: int(remaining.Seconds()), Limit: 1})
		cancel()
		if ctx.Err() != nil {
			break
		}
		var tr *errTransient
		if errors.As(err, &tr) {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
			continue
		}
		if err != nil {
			res.Note = "Waiting for the answer failed (" + err.Error() + "); check later with fleet_inbox(ticket=" + res.MessageID + ")."
			break
		}
		var ir fleet.InboxResult
		if json.Unmarshal(data, &ir) == nil && ir.Ticket != nil {
			res = *ir.Ticket
		}
		if res.Status != fleet.AskPending {
			break
		}
	}
	if res.Status == fleet.AskPending && res.Note == "" {
		res.Note = "No answer yet. It will arrive later as a message in this session; or check with fleet_inbox(ticket=" + res.MessageID + ")."
	}
	var text string
	switch res.Status {
	case fleet.AskReplied:
		text = fmt.Sprintf("Answer from %s:\n%s", res.ReplyFrom, res.Reply)
	default:
		b, _ := json.MarshalIndent(res, "", "  ")
		text = string(b)
	}
	return text, res, nil
}
