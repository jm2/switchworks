// SPDX-License-Identifier: GPL-3.0-or-later

// Package fleet defines the fleet operations agents call through `swyd mcp`
// (and people through the CLI): their arguments and results. The node
// forwards them to the coordinator, which implements them.
package fleet

import (
	"time"

	"github.com/jm2/switchyard/internal/model"
)

// Limits shared by the shim and the coordinator.
const (
	DefaultAskWait = 10 * time.Minute
	MaxAskWait     = 30 * time.Minute
	MaxTextBytes   = 64 << 10
)

type ListArgs struct {
	Host  string `json:"host,omitempty"`
	Label string `json:"label,omitempty"` // "key=value"
	All   bool   `json:"all,omitempty"`   // include stopped sessions
}

// SessionInfo is a session as agents see it.
type SessionInfo struct {
	ID      string            `json:"id"`
	Path    string            `json:"path"`
	Harness string            `json:"harness"`
	Status  string            `json:"status"`
	Online  bool              `json:"online"`
	Labels  map[string]string `json:"labels,omitempty"`
	Parent  string            `json:"parent,omitempty"`
	Model   string            `json:"model,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Usage   *model.Usage      `json:"usage,omitempty"`
	Caps    model.Caps        `json:"caps"`
}

type ListResult struct {
	Self     string        `json:"self"` // the calling session's path
	Sessions []SessionInfo `json:"sessions"`
}

type SendArgs struct {
	To         string `json:"to"`
	Text       string `json:"text"`
	Mode       string `json:"mode,omitempty"` // queue (default) | steer | interrupt
	Broadcast  bool   `json:"broadcast,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// MessageInfo summarizes a routed message.
type MessageInfo struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	State  string `json:"state"`
	Mode   string `json:"mode,omitempty"`
	Error  string `json:"error,omitempty"`
	Target string `json:"target,omitempty"` // resolved session path or user
}

type SendResult struct {
	Messages []MessageInfo `json:"messages"`
}

type AskArgs struct {
	To          string `json:"to"`
	Text        string `json:"text"`
	WaitSeconds *int   `json:"wait_seconds,omitempty"` // default 600; 0 = return a ticket at once
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
	// HoldSeconds, with WaitSeconds 0, says the caller will collect the
	// reply with fleet_inbox(ticket) within this long; a reply arriving
	// sooner is held for it instead of being delivered as a new turn.
	// (swyd mcp uses this to wait in a separate, retriable call.)
	HoldSeconds int `json:"hold_seconds,omitempty"`
}

// Ask statuses.
const (
	AskReplied = "replied"
	AskPending = "pending"
	AskFailed  = "failed"
)

type AskResult struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
	Target    string `json:"target,omitempty"`
	Reply     string `json:"reply,omitempty"`
	ReplyFrom string `json:"reply_from,omitempty"`
	Error     string `json:"error,omitempty"`
	Note      string `json:"note,omitempty"`
}

type ReplyArgs struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

type ReplyResult struct {
	ReplyID string `json:"reply_id"`
	State   string `json:"state"`  // a message state, or ReplyHeld
	Inline  bool   `json:"inline"` // handed straight to the asker's waiting fleet_ask
}

// ReplyHeld is the state of a reply kept for an asker that is about to
// collect it.
const ReplyHeld = "held"

type InboxArgs struct {
	Ticket      string `json:"ticket,omitempty"` // an ask id to check (and optionally wait on)
	WaitSeconds int    `json:"wait_seconds,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type InboxResult struct {
	Ticket   *AskResult      `json:"ticket,omitempty"`
	Messages []model.Message `json:"messages"`
}

type SpawnArgs struct {
	Host    string            `json:"host"`
	Cwd     string            `json:"cwd"`
	Prompt  string            `json:"prompt,omitempty"`
	Name    string            `json:"name,omitempty"`
	Harness string            `json:"harness,omitempty"`
	Model   string            `json:"model,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

type SpawnResult struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type StatusArgs struct {
	Session string `json:"session"`
}

type StatusResult struct {
	SessionInfo
	LastMessage string    `json:"last_message,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type StopArgs struct {
	Session string `json:"session"`
}

type StopResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}
