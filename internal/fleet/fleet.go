// SPDX-License-Identifier: GPL-3.0-or-later

// Package fleet defines the fleet operations agents call through `swk mcp`
// (and people through the CLI): their arguments and results. The node
// forwards them to the coordinator, which implements them.
package fleet

import (
	"time"

	"github.com/jm2/switchworks/internal/model"
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
	// Queued is how many messages wait for the session's next turn.
	Queued int `json:"queued,omitempty"`
}

type ListResult struct {
	Self     string        `json:"self"` // the calling session's path
	Hosts    []HostInfo    `json:"hosts"`
	Sessions []SessionInfo `json:"sessions"`
}

// HostInfo says which harnesses a host can start (for fleet_spawn).
type HostInfo struct {
	Host      string   `json:"host"`
	Online    bool     `json:"online"`
	Harnesses []string `json:"harnesses"`
}

type SendArgs struct {
	To         string `json:"to"`
	Text       string `json:"text"`
	Mode       string `json:"mode,omitempty"` // queue (default) | steer | interrupt
	Broadcast  bool   `json:"broadcast,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	// Images are image files on the sender's host, as an agent names them
	// in a fleet tool. The sender's node reads them (under the session's
	// working directory only) and replaces them with Attachments; they
	// never reach the coordinator.
	Images []string `json:"images,omitempty"`
	// Attachments carry the images' bytes to the coordinator.
	Attachments []model.Attachment `json:"attachments,omitempty"`
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
	// (swk mcp uses this to wait in a separate, retriable call.)
	HoldSeconds int `json:"hold_seconds,omitempty"`
	// Images and Attachments: as in SendArgs.
	Images      []string           `json:"images,omitempty"`
	Attachments []model.Attachment `json:"attachments,omitempty"`
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
	// Images and Attachments go with the prompt: as in SendArgs.
	Images      []string           `json:"images,omitempty"`
	Attachments []model.Attachment `json:"attachments,omitempty"`
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
