// SPDX-License-Identifier: GPL-3.0-or-later

// Package adapter defines the boundary between Switchyard and an agent
// harness (Codex, Antigravity, later ACP agents). Each harness implements
// Adapter; everything above it (node, coordinator, UI) only sees the
// normalized events in package model.
package adapter

import (
	"context"
	"errors"

	"github.com/jm2/switchyard/internal/model"
)

// Errors returned by handles.
var (
	ErrNotSupported = errors.New("operation not supported by this harness")
	ErrBusy         = errors.New("session is busy")
	ErrNotActive    = errors.New("no active turn")
	ErrUnknown      = errors.New("unknown approval id")
	ErrClosed       = errors.New("session closed")
)

// DeliveryMode says how a message enters a session.
type DeliveryMode string

const (
	// DeliverQueue starts a new turn; the caller must only use it when the
	// session is idle (ErrBusy otherwise).
	DeliverQueue DeliveryMode = "queue"
	// DeliverSteer injects input into the active turn (ErrNotActive if none).
	DeliverSteer DeliveryMode = "steer"
)

// Input is a message delivered into a session.
type Input struct {
	Text            string
	ClientMessageID string
}

// StartSpec describes a new or resumed session.
type StartSpec struct {
	Name           string // session path, e.g. "devbox/app-port"; used as the thread name
	Cwd            string
	Model          string
	Provider       string
	ApprovalPolicy string // codex: never | on-request | untrusted
	Sandbox        string // codex: read-only | workspace-write | danger-full-access
	Env            map[string]string
	// CwdRoot, when set, is the directory Cwd must stay inside (sessions
	// spawned by agents); adapters check it before every use of Cwd.
	CwdRoot string

	// MCP is an MCP server to attach to this session only (the fleet
	// tools). It carries a per-session credential, so it is never persisted.
	MCP *MCPServer `json:"-"`
}

// MCPServer is a stdio MCP server launched by the harness for one session.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	// ToolTimeout bounds one tool call (fleet_ask can wait for a long time).
	ToolTimeout int // seconds
}

// Event is what a handle reports; the node stamps id, seq and time.
type Event struct {
	Kind    string
	Payload any
}

// Sink receives a session's events, in order. It must not block for long.
type Sink func(Event)

// Adapter creates sessions for one harness on this host.
type Adapter interface {
	Name() string
	// Start creates a new harness session. It returns once the harness has
	// assigned its native id; Sink receives session.registered first.
	Start(ctx context.Context, spec StartSpec, sink Sink) (Handle, error)
	// Resume reattaches to an existing harness session after a node restart.
	Resume(ctx context.Context, ref model.NativeRef, spec StartSpec, sink Sink) (Handle, error)
	// Close stops every process the adapter owns.
	Close() error
}

// Handle is one live session.
type Handle interface {
	Ref() model.NativeRef
	Caps() model.Caps
	Status() string
	Deliver(ctx context.Context, in Input, mode DeliveryMode) error
	Interrupt(ctx context.Context) error
	Resolve(ctx context.Context, approvalID, decision string) error
	// Stop ends Switchyard's management of the session. The harness's own
	// history is kept.
	Stop(ctx context.Context) error
}

// Discoverer is implemented by adapters that can list sessions they did not
// start (observed mode). Not implemented yet.
type Discoverer interface {
	Discover(ctx context.Context) ([]model.NativeRef, error)
}
