// SPDX-License-Identifier: GPL-3.0-or-later

// Package fleet defines the fleet operations agents call through `swk mcp`
// (and people through the CLI): their arguments and results. The node
// forwards them to the coordinator, which implements them.
package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jm2/switchworks/internal/model"
)

// Limits shared by the shim and the coordinator.
const (
	DefaultAskWait = 10 * time.Minute
	MaxAskWait     = 30 * time.Minute
	MaxTextBytes   = 64 << 10
	// MaxSupersedesBytes bounds a supersedes key.
	MaxSupersedesBytes = 128
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

// MaxRecipients bounds the addresses of one send.
const MaxRecipients = 64

type SendArgs struct {
	// To is the recipient's address. In JSON, "to" may also be a list of
	// addresses, which fills ToList instead (a list of one fills To).
	To         string   `json:"to"`
	ToList     []string `json:"-"`
	Text       string   `json:"text"`
	Mode       string   `json:"mode,omitempty"` // auto (default) | queue | steer | interrupt
	Broadcast  bool     `json:"broadcast,omitempty"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
	// Supersedes is a key for what the message reports (e.g.
	// "status:gate5"): it replaces an earlier message with the same key
	// from the same sender to the same session that is still pending or
	// queued, which becomes superseded.
	Supersedes string `json:"supersedes,omitempty"`
	// Quiet: the message never starts a turn of its own. A running turn
	// takes it as usual (mode auto); an idle target keeps it queued until
	// its next turn starts for another reason. For acknowledgements and
	// FYIs that need no action now.
	Quiet bool `json:"quiet,omitempty"`
	// Images are image files on the sender's host, as an agent names them
	// in a fleet tool. The sender's node reads them (under the session's
	// working directory only) and replaces them with Attachments; they
	// never reach the coordinator.
	Images []string `json:"images,omitempty"`
	// Attachments carry the images' bytes to the coordinator.
	Attachments []model.Attachment `json:"attachments,omitempty"`
	// Files are text files on the sender's host (patches, logs), read by
	// the sender's node as Images are, into FileAttachments.
	Files           []string           `json:"files,omitempty"`
	FileAttachments []model.Attachment `json:"file_attachments,omitempty"`
}

// sendArgsJSON is SendArgs without its methods, for them to use.
type sendArgsJSON SendArgs

// UnmarshalJSON takes "to" as one address or a list of them. Unknown
// fields are refused, as the coordinator refuses them for every operation.
func (a *SendArgs) UnmarshalJSON(b []byte) error {
	var aux struct {
		sendArgsJSON
		To json.RawMessage `json:"to"`
	}
	if err := StrictUnmarshal(b, &aux); err != nil {
		return err
	}
	*a = SendArgs(aux.sendArgsJSON)
	var err error
	a.To, a.ToList, err = DecodeTo(aux.To)
	return err
}

// MarshalJSON writes "to" as a string unless ToList is set, so a send to
// one address reads the same to an older coordinator.
func (a SendArgs) MarshalJSON() ([]byte, error) {
	aux := struct {
		sendArgsJSON
		To any `json:"to"`
	}{sendArgsJSON(a), a.To}
	if len(a.ToList) > 0 {
		aux.To = a.ToList
	}
	return json.Marshal(aux)
}

// DecodeTo reads a "to" value: one address, or a list of addresses. A list
// of one comes back as one address; a longer list as many.
func DecodeTo(raw json.RawMessage) (one string, many []string, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	if raw[0] != '[' {
		if err := json.Unmarshal(raw, &one); err != nil {
			return "", nil, errors.New(`"to" must be an address or a list of addresses`)
		}
		return one, nil, nil
	}
	if err := json.Unmarshal(raw, &many); err != nil {
		return "", nil, errors.New(`"to" must be an address or a list of addresses`)
	}
	switch {
	case len(many) == 0:
		return "", nil, errors.New(`"to" is an empty list`)
	case len(many) == 1:
		return many[0], nil, nil
	case len(many) > MaxRecipients:
		return "", nil, fmt.Errorf(`"to" lists %d addresses; at most %d`, len(many), MaxRecipients)
	}
	return "", many, nil
}

// StrictUnmarshal decodes b into v, refusing unknown fields.
func StrictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Recipients lists a send's addresses: ToList, or To alone.
func (a SendArgs) Recipients() []string {
	if len(a.ToList) > 0 {
		return a.ToList
	}
	return []string{a.To}
}

// MessageInfo summarizes a routed message, or a recipient it could not go
// to (State Refused, no ID).
type MessageInfo struct {
	ID     string `json:"id,omitempty"`
	To     string `json:"to"`
	State  string `json:"state"`
	Mode   string `json:"mode,omitempty"`
	Error  string `json:"error,omitempty"`
	Target string `json:"target,omitempty"` // resolved session path or user
	// DeliveredAt is set when it went into the target's input at once (a
	// steer, or a person's inbox); a queued message has none yet.
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
}

// Refused is the State of a recipient a send to several could not reach:
// no message was stored for it.
const Refused = "refused"

type SendResult struct {
	Messages []MessageInfo `json:"messages"`
	// Group is shared by the messages of a send to several recipients.
	Group string `json:"group,omitempty"`
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
	// Files and FileAttachments: as in SendArgs.
	Files           []string           `json:"files,omitempty"`
	FileAttachments []model.Attachment `json:"file_attachments,omitempty"`
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
	// All lists recent messages to and from the session, read or not,
	// instead of only the unread ones.
	All bool `json:"all,omitempty"`
	// Verbose returns full message records instead of InboxMessage.
	Verbose bool `json:"verbose,omitempty"`
}

// Inbox limits.
const (
	DefaultInboxLimit = 20
	MaxInboxLimit     = 200
	// InboxTextBytes is the most of a message's text the inbox shows; the
	// whole text arrived as a turn.
	InboxTextBytes = 2000
)

type InboxResult struct {
	Ticket *AskResult `json:"ticket,omitempty"`
	// Messages are []InboxMessage, or []model.Message with Verbose. With
	// Ticket it is empty: checking an ask reads no messages.
	Messages any `json:"messages"`
	// More counts the unread messages left for the next call.
	More int `json:"more,omitempty"`
}

// InboxMessage is the compact form of a message in fleet_inbox.
type InboxMessage struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"` // only on messages the session sent
	CreatedAt time.Time `json:"created_at"`
	Text      string    `json:"text"`
	Re        string    `json:"re,omitempty"`      // the ask a reply answers
	ReText    string    `json:"re_text,omitempty"` // the start of that ask
	// On messages the session sent: their state (delivered, seen,
	// answered...), when they went into the target's input, and when the
	// turn that carried them finished.
	State       string    `json:"state,omitempty"`
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
	SeenAt      time.Time `json:"seen_at,omitzero"`
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
