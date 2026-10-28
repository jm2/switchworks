// SPDX-License-Identifier: GPL-3.0-or-later

// Package mcp is `swk mcp`: a stdio MCP server that gives an agent session
// its fleet tools (fleet_list, fleet_send, fleet_ask, fleet_reply,
// fleet_inbox, fleet_spawn, fleet_status, fleet_stop). The harness starts it
// inside the session with SWK_NODE_URL, SWK_SESSION and SWK_TOKEN set by
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

	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/gpurun"
	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/version"
)

// Protocol versions this server speaks, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Environment variables (set by the node; duplicated here to avoid an
// import cycle).
const (
	envNodeURL     = "SWK_NODE_URL"
	envSession     = "SWK_SESSION"
	envSessionPath = "SWK_SESSION_PATH"
	envToken       = "SWK_TOKEN"
	envGPURun      = "SWK_GPU_RUN"
)

// NodeClient calls the node's local API for one session.
type NodeClient struct {
	URL     string
	Session string
	Token   string
	HTTP    *http.Client
	// GPU is set when the node takes GPU runs: the server offers gpu_run.
	GPU bool
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
	return c.post(ctx, "/v1/fleet/"+op, args)
}

// RunGPU asks the node for a GPU run and waits for its result.
func (c *NodeClient) RunGPU(ctx context.Context, req gpurun.Request) (gpurun.Result, error) {
	var res gpurun.Result
	data, err := c.post(ctx, "/v1/gpu/run", req)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return res, fmt.Errorf("bad response from the swk node: %w", err)
	}
	return res, nil
}

func (c *NodeClient) post(ctx context.Context, path string, args any) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Swk-Session", c.Session)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &errTransient{msg: "cannot reach the swk node: " + err.Error()}
	}
	defer resp.Body.Close()
	var lr localResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&lr); err != nil {
		return nil, fmt.Errorf("bad response from the swk node (HTTP %d): %w", resp.StatusCode, err)
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

// imagesProp is the schema of the images argument.
var imagesProp = map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"},
	"description": "Image files to attach (screenshots, diagrams): paths of PNG, JPEG, WebP or GIF files inside your working directory (relative to it, or absolute), at most 4, 5 MiB each and 10 MiB in all. Symlinks and files outside your working directory are refused. The recipient's harness must take images (Codex does; agy does not)."}

// filesProp is the schema of the files argument.
var filesProp = map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"},
	"description": "Text files to attach (a patch from git format-patch, a log, notes): paths inside your working directory (relative to it, or absolute), at most 4, 1 MiB each and 2 MiB in all, UTF-8 text without NUL bytes. The recipient gets a copy of each and the copy's path in the message, so nothing needs copying by hand. Symlinks and files outside your working directory are refused."}

const addressHelp = `Addresses: "host/name" (a session path), a fleet-unique session name, "parent" (the session that spawned you), "@key=value" (sessions with that label; host and harness also work as keys), or "user:<name>" (a person, who reads it in the Switchworks UI or CLI).`

// Tools are the fleet tools.
var Tools = []Tool{
	{Name: "fleet_list", Title: "List fleet sessions", Annotations: map[string]bool{"readOnlyHint": true},
		Description: "List the agent sessions in the Switchworks fleet, across all hosts, with their status (and how many messages wait in each one's queue). Use a session's path (host/name) to address it in other fleet tools.",
		InputSchema: obj(map[string]any{
			"host":  str("Only sessions on this host."),
			"label": str(`Only sessions with this label, as "key=value".`),
			"all":   map[string]any{"type": "boolean", "description": "Include stopped sessions."},
		})},
	{Name: "fleet_send", Title: "Send a message",
		Description: "Send a message to another agent session (or a person) without waiting for an answer. " + addressHelp +
			` mode: "auto" (default: added to the target's running turn if it has one, at its next tool call; otherwise it starts a turn), "queue" (waits for the target's turn to end, then starts the next one together with any other waiting messages), "steer" (like auto; reaches a target that has opted out of auto only if you spawned it), "interrupt" (cancels the target's running turn and delivers now; only for sessions you spawned). Set broadcast to send to every session matching an @selector.` +
			` To tell several sessions the same thing, pass a list of addresses in one call rather than calling once per recipient: each gets its own message, and the result lists each one's id or why it could not go.` +
			` The result has delivered_at if it went straight into the target's running turn; fleet_inbox(all: true) shows later whether it was delivered and seen, so there is no need to resend.` +
			` For progress updates, give each one the same supersedes key (e.g. "status:gate5"): a newer update replaces your earlier one with that key if the target has not received it yet, so it reads only the latest. Undelivered messages expire after 2 hours by default; set ttl_seconds shorter for a note that is only useful soon, or longer (up to 24 hours) for one that must wait.`,
		InputSchema: obj(map[string]any{
			"to": map[string]any{"description": "Recipient address, or a list of up to 64 addresses.", "anyOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": fleet.MaxRecipients},
			}},
			"text":      str("The message."),
			"mode":      map[string]any{"type": "string", "enum": []string{"auto", "queue", "steer", "interrupt"}, "description": "Delivery mode (default auto)."},
			"broadcast": map[string]any{"type": "boolean", "description": "Send to every session matching an @selector."},
			"supersedes": map[string]any{"type": "string", "maxLength": fleet.MaxSupersedesBytes,
				"description": `A key for what this message reports, e.g. "status:gate5": it replaces your earlier message with the same key to the same session while that one still waits to be delivered.`},
			"ttl_seconds": map[string]any{"type": "integer", "minimum": 1, "description": "Expire undelivered after this many seconds (default: the fleet's agent_notify_ttl_seconds, 2 hours unless changed; at most its default_ttl_seconds, 24 hours unless changed)."},
			"images":      imagesProp,
			"files":       filesProp,
		}, "to", "text")},
	{Name: "fleet_ask", Title: "Ask and wait for an answer",
		Description: "Ask another agent session (or a person) a question and wait for the answer, for up to wait_seconds (default 600, max 1800). " + addressHelp +
			" The recipient answers with fleet_reply. If no answer arrives in time you get a ticket instead: the answer will arrive later as a message in this session, or check it with fleet_inbox(ticket).",
		InputSchema: obj(map[string]any{
			"to":           str("Recipient address."),
			"text":         str("The question."),
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 1800, "description": "How long to wait for the answer (default 600; 0 returns a ticket at once)."},
			"images":       imagesProp,
			"files":        filesProp,
		}, "to", "text")},
	{Name: "fleet_reply", Title: "Answer a question",
		Description: `Answer a question another session asked you. A question arrives as a message starting with "[switchworks msg=<id> ... kind=ask"; pass that id as message_id.`,
		InputSchema: obj(map[string]any{
			"message_id": str("The id of the ask you are answering."),
			"text":       str("Your answer."),
		}, "message_id", "text")},
	{Name: "fleet_inbox", Title: "Unread messages",
		Description: "Show messages sent to this session that you have not seen yet, oldest first: each as {id, kind, from, created_at, text, re}, text cut at 2,000 bytes. " +
			"You rarely need this: every message already arrives as a turn of its own, and those count as seen. Do not poll it. " +
			"all: true shows recent messages to and from this session, seen or not, with the state, delivered_at and seen_at of those you sent; verbose: true gives full records. " +
			"With ticket (an ask id returned by fleet_ask), report only whether that ask has been answered, waiting up to wait_seconds for it.",
		InputSchema: obj(map[string]any{
			"ticket":       str("An ask id to check."),
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 1800, "description": "With ticket: how long to wait for the answer."},
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "How many messages (default 20)."},
			"all":          map[string]any{"type": "boolean", "description": "Recent messages to and from this session, seen or not, newest first."},
			"verbose":      map[string]any{"type": "boolean", "description": "Full message records (addresses, state, hops, times)."},
		})},
	{Name: "fleet_spawn", Title: "Start a new agent session",
		Description: "Start a new agent session on a host to work on a task in parallel. It runs on its own; when it has finished all its work you receive a completion message containing its last reply. You can message it, check it with fleet_status, or stop it with fleet_stop. It runs with your approval policy and sandbox; by default it must be on your host, use your harness, and work inside your working directory.",
		InputSchema: obj(map[string]any{
			"host":    str("Host to run on (see fleet_list); default: your own."),
			"cwd":     str("Working directory on that host; default: your own."),
			"prompt":  str("The task."),
			"name":    str("Session name (default: derived from yours)."),
			"harness": map[string]any{"type": "string", "description": "Agent harness: codex (default), agy, goose, or another ACP agent the host offers (fleet_list shows each host's harnesses)."},
			"model":   str("Model override."),
			"labels":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Labels for @selectors."},
			"images":  imagesProp,
		}, "prompt")},
	{Name: "fleet_status", Title: "Session status", Annotations: map[string]bool{"readOnlyHint": true},
		Description: `Get a session's status, model and token usage, and the last message of yourself, your parent or a session you spawned. session: a path, a name, "parent", or omit it for yourself.`,
		InputSchema: obj(map[string]any{"session": str("The session (default: yourself).")})},
	{Name: "fleet_stop", Title: "Stop a session", Annotations: map[string]bool{"destructiveHint": true},
		Description: "Stop a session you spawned (its history is kept).",
		InputSchema: obj(map[string]any{"session": str("The session to stop.")}, "session")},
}

// gpuTool is offered when the node takes GPU runs (Windows nodes with
// gpu_run.enabled).
var gpuTool = Tool{Name: "gpu_run", Title: "Run a command with the GPU",
	Description: `Run a command where it can use the GPU, and get its exit code and output. Use it for work that fails in your sandbox for want of a GPU or a display: Direct3D 9/11/12, OpenGL or Vulkan programs, games and their tests, renderers, GPU benchmarks. Never work around the sandbox instead (no starting programs on the user's desktop by other means, no simulated keystrokes or mouse input).

The node runs the command on the user's interactive desktop, so its windows show on their screen, inside a Windows AppContainer: your working directory is readable and writable (every .git read-only, the deny list's files hidden), the user's profile and other files are out of reach, there is no network unless the node allows it (never loopback), no clipboard, no access to other programs' windows, no stdin. The command and everything it starts is killed when the command exits or its time limit passes. The program must run unattended: give it a test or scripted mode, and have it write screenshots or logs into your working directory to inspect afterwards. Only the last 64 KiB of stdout and of stderr come back. The environment is a fixed minimal one (system paths; TEMP, APPDATA and LOCALAPPDATA in the container's own folder) plus env.`,
	InputSchema: obj(map[string]any{
		"argv": map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "items": map[string]any{"type": "string"},
			"description": `The program and its arguments, e.g. ["build/game.exe", "--test-frames", "300"]. A bare name is looked up on the node's PATH; a path is relative to cwd. Batch files: ["cmd.exe", "/d", "/c", "run.bat"].`},
		"cwd":             str("Working directory, relative to yours (default: yours). It must be inside your working directory."),
		"env":             map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Environment variables to add."},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 1800, "description": "Time limit (default 300; the node may allow less)."},
	}, "argv"),
	Annotations: map[string]bool{"destructiveHint": true}}

func (s *Server) tools() []Tool {
	if s.node.GPU {
		return append(append([]Tool{}, Tools...), gpuTool)
	}
	return Tools
}

const instructions = `You are one agent session in a Switchworks fleet: other agents run in sessions on several machines. Use the fleet_* tools to see them (fleet_list), message them (fleet_send), ask them questions and wait for answers (fleet_ask), and start helpers (fleet_spawn). Messages from other sessions arrive as input that starts with "[switchworks msg=... from=... kind=...]"; one turn can carry several, each with its own header. When kind=ask, answer with fleet_reply using that msg id. Messages start turns on their own, so there is no need to poll fleet_inbox. When you report progress with fleet_send, give each update the same supersedes key (e.g. "status:gate5") so a newer one replaces an older one the target has not received yet, and set ttl_seconds on a note that is only useful for a short while.`

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

// Main implements `swk mcp`.
func Main(stdin io.Reader, stdout, stderr io.Writer) int {
	node := &NodeClient{URL: os.Getenv(envNodeURL), Session: os.Getenv(envSession), Token: os.Getenv(envToken),
		GPU: os.Getenv(envGPURun) == "1"}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, "swk mcp: "+f+"\n", a...) }
	if node.URL == "" || node.Session == "" || node.Token == "" {
		logf("missing %s/%s/%s: swk mcp is started by a swk node inside a managed session", envNodeURL, envSession, envToken)
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
		_ = s.conn.Reply(id, map[string]any{"tools": s.tools()})
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

// checkImages validates the images argument. The files themselves are
// read by the node, which confines them to the session's working
// directory (attach.ReadWithin); agents cannot pass image data directly.
func checkImages(paths []string, atts []model.Attachment) error {
	return checkPaths("images", paths, attach.MaxImages, len(atts) > 0)
}

// checkFiles validates the files argument, as checkImages does images
// (attach.ReadFileWithin reads them on the node).
func checkFiles(paths []string, atts []model.Attachment) error {
	return checkPaths("files", paths, attach.MaxFiles, len(atts) > 0)
}

func checkPaths(field string, paths []string, max int, data bool) error {
	if data {
		return fmt.Errorf("attach %s by file path, in %q", field, field)
	}
	if len(paths) > max {
		return fmt.Errorf("%d %s; at most %d per message", len(paths), field, max)
	}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" || strings.ContainsRune(p, 0) || len(p) > 4096 {
			return fmt.Errorf("%s: each entry must be a file path", field)
		}
	}
	return nil
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
		if err := checkImages(a.Images, a.Attachments); err != nil {
			return "", nil, err
		}
		if err := checkFiles(a.Files, a.FileAttachments); err != nil {
			return "", nil, err
		}
		op, args = "send", a
	case "fleet_ask":
		a, err := decode[fleet.AskArgs](raw)
		if err != nil {
			return "", nil, err
		}
		if err := checkImages(a.Images, a.Attachments); err != nil {
			return "", nil, err
		}
		if err := checkFiles(a.Files, a.FileAttachments); err != nil {
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
		if err := checkImages(a.Images, a.Attachments); err != nil {
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
	case "gpu_run":
		if !s.node.GPU {
			return "", nil, fmt.Errorf("unknown tool %q", name)
		}
		return s.gpuRun(ctx, raw)
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

// gpuRun asks the node for a GPU run. It is not retried: the command may
// have run.
func (s *Server) gpuRun(ctx context.Context, raw json.RawMessage) (string, any, error) {
	var req gpurun.Request
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) == 0 || dec.Decode(&req) != nil {
		return "", nil, errors.New(`bad arguments: give argv, a list of strings, and optionally cwd, env and timeout_seconds`)
	}
	if err := gpurun.Validate(req, 24*time.Hour); err != nil { // the node applies its own limit
		return "", nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, req.Timeout()+2*time.Minute)
	defer cancel()
	res, err := s.node.RunGPU(cctx, req)
	if err != nil {
		return "", nil, err
	}
	return FormatGPUResult(res), res, nil
}

// FormatGPUResult is a run's result as the model reads it.
func FormatGPUResult(r gpurun.Result) string {
	var b strings.Builder
	code := fmt.Sprintf("%d", r.ExitCode)
	if r.ExitCode >= 0x80000000 {
		code += fmt.Sprintf(" (0x%08X)", uint32(r.ExitCode))
	}
	if r.TimedOut {
		fmt.Fprintf(&b, "Timed out after %.1fs and was killed (exit code %s).\n", float64(r.DurationMS)/1000, code)
	} else {
		fmt.Fprintf(&b, "Exit code %s after %.1fs.\n", code, float64(r.DurationMS)/1000)
	}
	if r.Skipped > 0 {
		fmt.Fprintf(&b, "%d workspace entries were not made reachable (links, hard links, or ACLs the node cannot change).\n", r.Skipped)
	}
	for _, st := range []struct {
		name    string
		text    string
		dropped int64
	}{{"stdout", r.Stdout, r.StdoutDropped}, {"stderr", r.Stderr, r.StderrDropped}} {
		if st.text == "" && st.dropped == 0 {
			fmt.Fprintf(&b, "\n%s: (empty)\n", st.name)
			continue
		}
		if st.dropped > 0 {
			fmt.Fprintf(&b, "\n%s (last %d bytes; %d before them dropped):\n", st.name, len(st.text), st.dropped)
		} else {
			fmt.Fprintf(&b, "\n%s:\n", st.name)
		}
		b.WriteString(st.text)
		if !strings.HasSuffix(st.text, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
