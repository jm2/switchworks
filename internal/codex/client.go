// SPDX-License-Identifier: GPL-3.0-or-later

// Package codex drives `codex app-server` over stdio JSON-RPC.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/version"
)

// Handler receives server-initiated traffic. Calls arrive sequentially, in
// wire order, from the connection's read goroutine.
type Handler interface {
	OnNotification(method string, params json.RawMessage)
	// OnServerRequest must eventually answer through Client.Reply or
	// Client.ReplyError; it must not block.
	OnServerRequest(id json.RawMessage, method string, params json.RawMessage)
}

// Options configure how app-server is launched.
type Options struct {
	Bin    string            // default "codex"
	Args   []string          // default ["app-server"]
	Dir    string            // working directory of the app-server process
	Env    map[string]string // environment overrides (e.g. CODEX_HOME)
	Stderr io.Writer         // app-server logs; nil discards
	Tap    jsonrpc.Tap       // raw protocol lines, for capture/debugging

	// OptOut suppresses notifications we never use.
	OptOut []string
}

// Client is a live app-server connection.
type Client struct {
	proc *proc.Process
	conn *jsonrpc.Conn
	Info InitializeResponse
}

type handlerAdapter struct{ h Handler }

func (a handlerAdapter) HandleNotification(method string, params json.RawMessage) {
	if a.h != nil {
		a.h.OnNotification(method, params)
	}
}

func (a handlerAdapter) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	if a.h != nil {
		a.h.OnServerRequest(id, method, params)
	}
}

// Start launches app-server and completes the initialize handshake.
func Start(ctx context.Context, opts Options, h Handler) (*Client, error) {
	bin := opts.Bin
	if bin == "" {
		bin = "codex"
	}
	args := opts.Args
	if len(args) == 0 {
		args = []string{"app-server"}
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// The end of app-server's stderr says why a start failed: a bad
	// argument or config.toml makes it exit before it answers initialize.
	tail := &tailBuffer{max: 2048}
	p, err := proc.Start(bin, args, proc.Options{Dir: opts.Dir, Env: proc.Environ(opts.Env), Stderr: io.MultiWriter(stderr, tail)})
	if err != nil {
		return nil, fmt.Errorf("start %s app-server: %w", bin, err)
	}
	c := &Client{proc: p}
	c.conn = jsonrpc.NewConn(p.Stdout, p.Stdin, handlerAdapter{h}, opts.Tap)

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	params := InitializeParams{
		ClientInfo: ClientInfo{Name: version.Slug, Title: version.Name, Version: version.Version},
		Capabilities: &InitializeCapabilities{
			ExperimentalAPI:           true,
			OptOutNotificationMethods: opts.OptOut,
		},
	}
	if err := c.conn.Call(initCtx, MethodInitialize, params, &c.Info); err != nil {
		_ = p.Kill() // waits for the exit, and so for the last of stderr
		return nil, &StartError{Err: fmt.Errorf("app-server initialize: %w", err), Stderr: tail.String()}
	}
	if err := c.conn.Notify(NotifyInitialized, nil); err != nil {
		_ = p.Kill()
		return nil, fmt.Errorf("app-server initialized: %w", err)
	}
	return c, nil
}

// StartError is a failed app-server start, with the end of what the
// process wrote to stderr. Its message leaves stderr out (it may quote
// config.toml, and errors travel to the coordinator): log Stderr locally.
type StartError struct {
	Err    error
	Stderr string
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(t.b), "�"))
}

// Done is closed when the app-server process exits or its stdout closes.
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// Close stops the app-server process.
func (c *Client) Close() error { return c.proc.Stop(4 * time.Second) }

// Kill ends the app-server process tree at once.
func (c *Client) Kill() error { return c.proc.Kill() }

// ExitErr returns how the app-server process exited, once it has: nil for
// exit code 0.
func (c *Client) ExitErr() error { return c.proc.Err() }

// Pid returns the app-server process id.
func (c *Client) Pid() int { return c.proc.Pid() }

// Reply answers a server-initiated request.
func (c *Client) Reply(id json.RawMessage, result any) error { return c.conn.Reply(id, result) }

// ReplyError rejects a server-initiated request.
func (c *Client) ReplyError(id json.RawMessage, code int, msg string) error {
	return c.conn.ReplyError(id, code, msg)
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	return c.conn.CallRetry(ctx, method, params, result)
}

func (c *Client) ThreadStart(ctx context.Context, p ThreadStartParams) (*ThreadStartResponse, error) {
	var r ThreadStartResponse
	if err := c.call(ctx, MethodThreadStart, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ThreadResume(ctx context.Context, p ThreadResumeParams) (*ThreadResumeResponse, error) {
	var r ThreadResumeResponse
	if err := c.call(ctx, MethodThreadResume, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ThreadSetName(ctx context.Context, threadID, name string) error {
	return c.call(ctx, MethodThreadSetName, ThreadSetNameParams{ThreadID: threadID, Name: name}, nil)
}

func (c *Client) ThreadList(ctx context.Context, p ThreadListParams) (*ThreadListResponse, error) {
	var r ThreadListResponse
	if err := c.call(ctx, MethodThreadList, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ThreadRead(ctx context.Context, p ThreadReadParams) (*ThreadReadResponse, error) {
	var r ThreadReadResponse
	if err := c.call(ctx, MethodThreadRead, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ThreadUnsubscribe(ctx context.Context, threadID string) error {
	return c.call(ctx, MethodThreadUnsubscribe, ThreadUnsubscribeParams{ThreadID: threadID}, nil)
}

// BackgroundTerminals lists the thread's commands that are still running.
func (c *Client) BackgroundTerminals(ctx context.Context, threadID string) ([]BackgroundTerminal, error) {
	var r BackgroundTerminalsListResponse
	if err := c.call(ctx, MethodBackgroundTerminalsList, BackgroundTerminalsListParams{ThreadID: threadID}, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

func (c *Client) TerminateBackgroundTerminal(ctx context.Context, threadID, processID string) error {
	return c.call(ctx, MethodBackgroundTerminalsTerminate, BackgroundTerminalsTerminateParams{ThreadID: threadID, ProcessID: processID}, nil)
}

// CleanBackgroundTerminals stops every command still running for the thread.
func (c *Client) CleanBackgroundTerminals(ctx context.Context, threadID string) error {
	return c.call(ctx, MethodBackgroundTerminalsClean, BackgroundTerminalsCleanParams{ThreadID: threadID}, nil)
}

func (c *Client) TurnStart(ctx context.Context, p TurnStartParams) (*TurnStartResponse, error) {
	var r TurnStartResponse
	if err := c.call(ctx, MethodTurnStart, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) TurnSteer(ctx context.Context, p TurnSteerParams) (*TurnSteerResponse, error) {
	var r TurnSteerResponse
	if err := c.call(ctx, MethodTurnSteer, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	return c.call(ctx, MethodTurnInterrupt, TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil)
}
