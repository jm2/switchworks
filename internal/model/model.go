// SPDX-License-Identifier: GPL-3.0-or-later

// Package model holds the records shared by node, coordinator and CLI: hosts,
// sessions, events and their normalized payloads.
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Host is an enrolled workstation running `swk node`.
type Host struct {
	ID       string   `json:"id"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
	Adapters []string `json:"adapters"`
	// Images lists the harnesses on the host that can take image
	// attachments, as its node last reported. An ACP agent is listed when
	// the node supports images for it; each session's Caps.Images says
	// whether its agent actually advertises image prompts.
	Images    []string  `json:"images,omitempty"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen"`
	// CertSHA256 is the fingerprint of the host's current node certificate.
	// Only that certificate may connect: enrolling again replaces it, and
	// removing the host revokes it.
	CertSHA256 string `json:"cert_sha256,omitempty"`
}

// Session modes.
const (
	ModeManaged  = "managed"
	ModeObserved = "observed"
)

// Session statuses.
const (
	StatusStarting        = "starting"
	StatusIdle            = "idle"
	StatusActive          = "active"
	StatusWaitingApproval = "waiting_approval"
	StatusError           = "error"
	StatusStopped         = "stopped"
	StatusGone            = "gone"
	// StatusOffline means the session's node is disconnected; it is a
	// coordinator-side view, never emitted by a node.
	StatusOffline = "offline"
)

// Caps are the operations a session supports.
type Caps struct {
	Message   bool `json:"message"`
	Steer     bool `json:"steer"`
	Interrupt bool `json:"interrupt"`
	Stop      bool `json:"stop"`
	Approve   bool `json:"approve"`
	ReadOnly  bool `json:"read_only"`
	// Images: the session accepts image attachments (Codex, and ACP agents
	// whose initialize response advertises promptCapabilities.image).
	Images bool `json:"images,omitempty"`
}

// NativeRef identifies a session inside its harness.
type NativeRef struct {
	Harness string `json:"harness"`
	ID      string `json:"id"`             // e.g. Codex thread id, agy conversation id
	Home    string `json:"home,omitempty"` // e.g. CODEX_HOME on that host
}

// Usage is the latest known token usage of a session.
type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
}

// Session is one agent conversation on one host.
type Session struct {
	ID        string            `json:"id"`
	Path      string            `json:"path"` // host/name
	Host      string            `json:"host"`
	Name      string            `json:"name"`
	Harness   string            `json:"harness"`
	Mode      string            `json:"mode"`
	NativeRef *NativeRef        `json:"native_ref,omitempty"`
	Cwd       string            `json:"cwd"`
	Labels    map[string]string `json:"labels,omitempty"`
	Caps      Caps              `json:"caps"`
	Status    string            `json:"status"`
	Parent    string            `json:"parent,omitempty"`
	// ApprovalPolicy and Sandbox are what the session runs with, as its node
	// reported at spawn; sessions it spawns inherit them.
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	// PoliciesReported: the node, not the coordinator, chose them (its
	// defaults), so they are not trusted beyond that node's own host.
	PoliciesReported bool      `json:"policies_reported,omitempty"`
	Model            string    `json:"model,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	Usage            *Usage    `json:"usage,omitempty"`
	LastSeq          int64     `json:"last_seq"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SpawnSpec is what a client asks for when starting a managed session.
type SpawnSpec struct {
	Host           string            `json:"host"`
	Name           string            `json:"name"`
	Harness        string            `json:"harness"` // "codex" (default), "agy", "goose" or a configured ACP agent
	Cwd            string            `json:"cwd"`
	Prompt         string            `json:"prompt,omitempty"`
	Attachments    []Attachment      `json:"attachments,omitempty"` // images sent with the prompt
	Model          string            `json:"model,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	ApprovalPolicy string            `json:"approval_policy,omitempty"`
	Sandbox        string            `json:"sandbox,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Parent         string            `json:"parent,omitempty"`
	// CwdRoot, set by the coordinator for spawns by agents, is the
	// directory Cwd must be inside; the node checks it with symlinks
	// resolved.
	CwdRoot string `json:"cwd_root,omitempty"`
}

// Event is one entry in a session's append-only log.
type Event struct {
	ID        string          `json:"id"` // ULID, assigned by the producer
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"` // per session, starting at 1, no gaps
	TS        time.Time       `json:"ts"`
	Kind      string          `json:"kind"`
	Source    string          `json:"source"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Event kinds.
const (
	KindSessionRegistered = "session.registered"
	KindSessionStatus     = "session.status"
	KindTurnStarted       = "turn.started"
	KindTurnCompleted     = "turn.completed"
	KindItemStarted       = "item.started"
	KindItemDelta         = "item.delta"
	KindItemCompleted     = "item.completed"
	KindTurnDiff          = "turn.diff"
	KindApprovalRequested = "approval.requested"
	KindApprovalResolved  = "approval.resolved"
	KindMessageQueued     = "message.queued"
	KindMessageDelivered  = "message.delivered"
	KindMessageFailed     = "message.failed"
	KindUsage             = "usage"
	KindError             = "error"
	KindWarning           = "warning"
)

// --- payloads ---

type SessionRegistered struct {
	NativeRef NativeRef `json:"native_ref"`
	Cwd       string    `json:"cwd,omitempty"`
	Model     string    `json:"model,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Caps      Caps      `json:"caps"`
	Resumed   bool      `json:"resumed,omitempty"`
}

type SessionStatus struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type TurnStarted struct {
	TurnID string `json:"turn_id"`
}

type TurnCompleted struct {
	TurnID     string `json:"turn_id"`
	Status     string `json:"status"` // completed | interrupted | failed
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

type Item struct {
	TurnID   string          `json:"turn_id"`
	ItemID   string          `json:"item_id"`
	ItemType string          `json:"item_type"`
	Text     string          `json:"text,omitempty"` // agent message text, when known
	Raw      json.RawMessage `json:"raw,omitempty"`  // harness-native item
}

type ItemDelta struct {
	TurnID string `json:"turn_id"`
	ItemID string `json:"item_id"`
	Text   string `json:"text"`
}

type TurnDiff struct {
	TurnID string `json:"turn_id"`
	Diff   string `json:"diff"`
}

type ApprovalRequested struct {
	ApprovalID string `json:"approval_id"`
	Kind       string `json:"kind"` // command | file_change
	TurnID     string `json:"turn_id,omitempty"`
	ItemID     string `json:"item_id,omitempty"`
	Command    string `json:"command,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// GrantRoot is a directory a file-change approval would also let the
	// agent write under for the rest of the session (Codex).
	GrantRoot string `json:"grant_root,omitempty"`
	// Decisions lists the decisions the harness offers for this prompt, when
	// it says (e.g. Codex may omit "decline"). Empty means the harness
	// default set.
	Decisions []string `json:"decisions,omitempty"`
}

type ApprovalResolved struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
}

type MessageEvent struct {
	MessageID string `json:"message_id"`
	From      string `json:"from"`
	Text      string `json:"text,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Error     string `json:"error,omitempty"`
	// Attachments: metadata only, never the image bytes.
	Attachments []Attachment `json:"attachments,omitempty"`
}

type UsageEvent struct {
	TurnID        string `json:"turn_id,omitempty"`
	Model         string `json:"model,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Last          Usage  `json:"last"`
	Total         Usage  `json:"total"`
	ContextWindow int64  `json:"context_window,omitempty"`
	// Restarted marks the first report of an adapter whose Total starts
	// over (a new process or handle), so an equal Total is still new usage.
	Restarted bool `json:"restarted,omitempty"`
}

type ErrorEvent struct {
	TurnID    string `json:"turn_id,omitempty"`
	Message   string `json:"message"`
	WillRetry bool   `json:"will_retry,omitempty"`
}

// --- names ---

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateName checks a host or session name segment.
func ValidateName(kind, s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("invalid %s name %q: use lowercase letters, digits, '.', '_' or '-' (max 63, must start with a letter or digit)", kind, s)
	}
	return nil
}

// ValidateSessionName allows extra path segments ("team/worker-3").
func ValidateSessionName(s string) error {
	parts := strings.Split(s, "/")
	if len(parts) > 4 {
		return fmt.Errorf("invalid session name %q: at most 4 segments", s)
	}
	for _, p := range parts {
		if err := ValidateName("session", p); err != nil {
			return err
		}
	}
	return nil
}

// JoinPath builds "host/name".
func JoinPath(host, name string) string { return host + "/" + name }

// Marshal encodes a payload, panicking on programmer error.
func Marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("model: marshal %T: %v", v, err))
	}
	return b
}
