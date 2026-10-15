// SPDX-License-Identifier: GPL-3.0-or-later

package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jm2/switchyard/internal/jsonrpc"
	"github.com/jm2/switchyard/internal/proc"
	"github.com/jm2/switchyard/internal/version"
)

// Handler receives agent-initiated traffic, sequentially and in wire order,
// from the connection's read goroutine. It must not block.
type Handler interface {
	// OnUpdate receives session/update notifications.
	OnUpdate(n SessionNotification)
	// OnPermission receives session/request_permission. The handler must
	// eventually answer with Client.Permit or Client.ReplyError.
	OnPermission(id json.RawMessage, req RequestPermissionRequest)
}

// Options configure how the agent is launched.
type Options struct {
	Bin    string
	Args   []string
	Dir    string            // working directory of the agent process
	Env    map[string]string // environment overrides
	Stderr io.Writer
	Tap    jsonrpc.Tap
}

// Client is a live connection to one agent process.
type Client struct {
	proc *proc.Process
	conn *jsonrpc.Conn
	h    Handler
	Info InitializeResponse
}

type connHandler struct{ c *Client }

func (ch connHandler) HandleNotification(method string, params json.RawMessage) {
	if method != MethodSessionUpdate {
		return
	}
	var n SessionNotification
	if json.Unmarshal(params, &n) == nil && ch.c.h != nil {
		ch.c.h.OnUpdate(n)
	}
}

func (ch connHandler) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	if method == MethodRequestPermission && ch.c.h != nil {
		var r RequestPermissionRequest
		if err := json.Unmarshal(params, &r); err != nil {
			_ = ch.c.conn.ReplyError(id, jsonrpc.CodeInvalidParams, "switchyard: bad permission request")
			return
		}
		ch.c.h.OnPermission(id, r)
		return
	}
	// Switchyard advertises no fs or terminal capability, so the agent does
	// its own file and shell work; anything else is unsupported.
	_ = ch.c.conn.ReplyError(id, jsonrpc.CodeMethodNotFound, "switchyard does not support "+method)
}

// Start launches the agent and completes the initialize handshake.
func Start(ctx context.Context, opts Options, h Handler) (*Client, error) {
	if opts.Bin == "" {
		return nil, fmt.Errorf("acp: no agent binary configured")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	p, err := proc.Start(opts.Bin, opts.Args, proc.Options{Dir: opts.Dir, Env: proc.Environ(opts.Env), Stderr: stderr})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", opts.Bin, err)
	}
	c := &Client{proc: p, h: h}
	c.conn = jsonrpc.NewConnWith(p.Stdout, p.Stdin, connHandler{c}, opts.Tap, jsonrpc.Options{VersionField: true})

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientInfo:      &Implementation{Name: version.Slug, Title: version.Name, Version: version.Version},
	}
	if err := c.conn.Call(initCtx, MethodInitialize, req, &c.Info); err != nil {
		_ = p.Kill()
		return nil, fmt.Errorf("%s initialize: %w", opts.Bin, err)
	}
	if c.Info.ProtocolVersion != ProtocolVersion {
		_ = p.Kill()
		return nil, fmt.Errorf("%s speaks ACP version %d; switchyard speaks %d", opts.Bin, c.Info.ProtocolVersion, ProtocolVersion)
	}
	return c, nil
}

// Done is closed when the agent's stdout closes (normally: it exited).
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// Close stops the agent process.
func (c *Client) Close() error { return c.proc.Stop(4 * time.Second) }

// Kill stops the agent process at once.
func (c *Client) Kill() error { return c.proc.Kill() }

// Pid returns the agent's process id.
func (c *Client) Pid() int { return c.proc.Pid() }

// NewSession creates a session.
func (c *Client) NewSession(ctx context.Context, r NewSessionRequest) (*SessionSetup, error) {
	var out SessionSetup
	if err := c.conn.Call(ctx, MethodSessionNew, r, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" {
		return nil, fmt.Errorf("session/new returned no sessionId")
	}
	return &out, nil
}

// LoadSession reopens a session; the agent replays its history as
// session/update notifications before answering.
func (c *Client) LoadSession(ctx context.Context, r LoadSessionRequest) (*SessionSetup, error) {
	var out SessionSetup
	if err := c.conn.Call(ctx, MethodSessionLoad, r, &out); err != nil {
		return nil, err
	}
	out.SessionID = r.SessionID
	return &out, nil
}

// ResumeSession reopens a session without replaying history.
func (c *Client) ResumeSession(ctx context.Context, r LoadSessionRequest) (*SessionSetup, error) {
	var out SessionSetup
	if err := c.conn.Call(ctx, MethodSessionResume, r, &out); err != nil {
		return nil, err
	}
	out.SessionID = r.SessionID
	return &out, nil
}

// Turn is a prompt that has been sent; Wait returns when the turn ends.
type Turn struct{ p *jsonrpc.Pending }

// Prompt sends a turn's prompt. It returns once the request is written, so
// a later Cancel is guaranteed to reach the agent after it.
func (c *Client) Prompt(sessionID, text string) (*Turn, error) {
	p, err := c.conn.Send(MethodSessionPrompt, PromptRequest{SessionID: sessionID,
		Prompt: []ContentBlock{{Type: "text", Text: text}}})
	if err != nil {
		return nil, err
	}
	return &Turn{p}, nil
}

// Wait waits for the end of the turn.
func (t *Turn) Wait(ctx context.Context) (*PromptResponse, error) {
	var out PromptResponse
	if err := t.p.Wait(ctx, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel asks the agent to stop the running turn.
func (c *Client) Cancel(sessionID string) error {
	return c.conn.Notify(MethodSessionCancel, CancelNotification{SessionID: sessionID})
}

// CloseSession releases a session (only when the agent supports it).
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	return c.conn.Call(ctx, MethodSessionClose, CloseSessionRequest{SessionID: sessionID}, nil)
}

// SetMode switches the session mode.
func (c *Client) SetMode(ctx context.Context, sessionID, mode string) error {
	return c.conn.Call(ctx, MethodSessionSetMode, SetModeRequest{SessionID: sessionID, ModeID: mode}, nil)
}

// SetConfigOption sets a select option and returns the updated options.
func (c *Client) SetConfigOption(ctx context.Context, sessionID, id, value string) ([]ConfigOption, error) {
	var out SetConfigOptionResponse
	if err := c.conn.Call(ctx, MethodSetConfigOption, SetConfigOptionRequest{SessionID: sessionID, ConfigID: id, Value: value}, &out); err != nil {
		return nil, err
	}
	return out.ConfigOptions, nil
}

// Permit answers a permission request.
func (c *Client) Permit(id json.RawMessage, outcome PermissionOutcome) error {
	return c.conn.Reply(id, RequestPermissionResponse{Outcome: outcome})
}

// ReplyError rejects an agent-initiated request.
func (c *Client) ReplyError(id json.RawMessage, code int, msg string) error {
	return c.conn.ReplyError(id, code, msg)
}
