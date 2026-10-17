// SPDX-License-Identifier: GPL-3.0-or-later

// Package acp drives an Agent Client Protocol agent (Goose, Crush, opencode,
// ...) over stdio JSON-RPC 2.0, as its client.
//
// Only the subset Switchworks uses is declared. Names and shapes follow the
// ACP v1 schema (agentclientprotocol/agent-client-protocol, schema/v1),
// checked against traffic recorded from goose 1.54.0. Fields the schema marks
// unstable (PromptResponse.usage) are decoded when present and never relied
// on.
package acp

import (
	"encoding/json"
	"strings"
)

// ProtocolVersion is the ACP major version this client speaks.
const ProtocolVersion = 1

// Agent methods (client to agent).
const (
	MethodInitialize      = "initialize"
	MethodSessionNew      = "session/new"
	MethodSessionLoad     = "session/load"
	MethodSessionResume   = "session/resume"
	MethodSessionPrompt   = "session/prompt"
	MethodSessionCancel   = "session/cancel" // notification
	MethodSessionClose    = "session/close"
	MethodSessionSetMode  = "session/set_mode"
	MethodSetConfigOption = "session/set_config_option"
)

// Client methods (agent to client).
const (
	MethodSessionUpdate     = "session/update" // notification
	MethodRequestPermission = "session/request_permission"

	// MethodGooseSessionUpdate is goose's own session update notification
	// (goose 1.55), sent only to clients that opt in with GooseCapsMeta.
	MethodGooseSessionUpdate = "_goose/unstable/session/update"
)

// --- initialize ---

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs"`
	Terminal bool                   `json:"terminal"`
	Meta     map[string]any         `json:"_meta,omitempty"`
}

// GooseCapsMeta asks goose for its custom notifications, the only place
// it reports a session's accumulated token usage. Other agents ignore
// _meta keys they do not know.
func GooseCapsMeta() map[string]any {
	return map[string]any{"goose": map[string]any{"customNotifications": true}}
}

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

// SessionCapabilities: a present (non-null) member means supported.
type SessionCapabilities struct {
	List   json.RawMessage `json:"list,omitempty"`
	Resume json.RawMessage `json:"resume,omitempty"`
	Close  json.RawMessage `json:"close,omitempty"`
}

func present(r json.RawMessage) bool { return len(r) > 0 && string(r) != "null" }

type McpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// PromptCapabilities says which content an agent accepts in session/prompt
// besides text (goose 1.54: image and embeddedContext).
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	McpCapabilities     McpCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
}

// CanResume reports whether the agent supports session/resume.
func (r InitializeResponse) CanResume() bool {
	return present(r.AgentCapabilities.SessionCapabilities.Resume)
}

// CanClose reports whether the agent supports session/close.
func (r InitializeResponse) CanClose() bool {
	return present(r.AgentCapabilities.SessionCapabilities.Close)
}

// AgentName is the agent's self-reported name and version, if any.
func (r InitializeResponse) AgentName() string {
	if r.AgentInfo == nil {
		return ""
	}
	if r.AgentInfo.Version == "" {
		return r.AgentInfo.Name
	}
	return r.AgentInfo.Name + " " + r.AgentInfo.Version
}

// --- sessions ---

// EnvVariable is how ACP passes MCP server environment.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// McpServerStdio is the stdio MCP transport every agent must support.
type McpServerStdio struct {
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Env     []EnvVariable `json:"env"`
}

type NewSessionRequest struct {
	Cwd        string           `json:"cwd"`
	McpServers []McpServerStdio `json:"mcpServers"`
}

type LoadSessionRequest struct {
	SessionID  string           `json:"sessionId"`
	Cwd        string           `json:"cwd"`
	McpServers []McpServerStdio `json:"mcpServers"`
}

type SessionMode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

type ConfigOptionValue struct {
	Value string `json:"value"`
	Name  string `json:"name,omitempty"`
}

// ConfigOption is a session configuration option ("select" options carry
// their values; groups are flattened when decoded).
type ConfigOption struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	CurrentValue json.RawMessage     `json:"currentValue,omitempty"`
	Options      []ConfigOptionValue `json:"-"`
}

func (o *ConfigOption) UnmarshalJSON(b []byte) error {
	type plain ConfigOption
	var raw struct {
		plain
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*o = ConfigOption(raw.plain)
	// options: a flat list of values, or a list of groups with "options".
	var flat []struct {
		ConfigOptionValue
		Options []ConfigOptionValue `json:"options"`
	}
	if json.Unmarshal(raw.Options, &flat) == nil {
		for _, v := range flat {
			if v.Value != "" {
				o.Options = append(o.Options, v.ConfigOptionValue)
			}
			o.Options = append(o.Options, v.Options...)
		}
	}
	return nil
}

// Current returns the current value of a select option.
func (o ConfigOption) Current() string {
	var s string
	_ = json.Unmarshal(o.CurrentValue, &s)
	return s
}

// Has reports whether a select option offers value.
func (o ConfigOption) Has(value string) bool {
	for _, v := range o.Options {
		if v.Value == value {
			return true
		}
	}
	return false
}

// SessionSetup is the common result of session/new, session/load and
// session/resume.
type SessionSetup struct {
	SessionID     string            `json:"sessionId,omitempty"` // session/new only
	Modes         *SessionModeState `json:"modes,omitempty"`
	ConfigOptions []ConfigOption    `json:"configOptions,omitempty"`
}

// Option returns the config option with id, if offered.
func (s *SessionSetup) Option(id string) *ConfigOption {
	for i := range s.ConfigOptions {
		if s.ConfigOptions[i].ID == id {
			return &s.ConfigOptions[i]
		}
	}
	return nil
}

// HasMode reports whether the agent offers a session mode.
func (s *SessionSetup) HasMode(id string) bool {
	if s.Modes == nil {
		return false
	}
	for _, m := range s.Modes.AvailableModes {
		if m.ID == id {
			return true
		}
	}
	return false
}

type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type SetConfigOptionRequest struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type SetConfigOptionResponse struct {
	ConfigOptions []ConfigOption `json:"configOptions"`
}

// --- prompt turn ---

// ContentBlock is a content block. Switchworks produces text, and image
// blocks for agents that advertise promptCapabilities.image.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`      // resource_link
	Name     string `json:"name,omitempty"`     // resource_link
	Data     string `json:"data,omitempty"`     // image: base64
	MimeType string `json:"mimeType,omitempty"` // image
}

type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// Stop reasons.
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// Usage is cumulative token usage for the session (unstable in the spec).
type Usage struct {
	TotalTokens      int64  `json:"totalTokens"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	ThoughtTokens    *int64 `json:"thoughtTokens,omitempty"`
	CachedReadTokens *int64 `json:"cachedReadTokens,omitempty"`
}

type PromptResponse struct {
	StopReason string `json:"stopReason"`
	Usage      *Usage `json:"usage,omitempty"`
}

type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

type CloseSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// --- session/update ---

// Session update kinds (the sessionUpdate discriminator).
const (
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdatePlan              = "plan"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
	UpdateConfigOption      = "config_option_update"
	UpdateSessionInfo       = "session_info_update"
	UpdateUsage             = "usage_update"
	UpdateNotice            = "notice"
	UpdateCompaction        = "compaction_update"
	UpdateCompactionSummary = "compaction_summary_chunk"
)

// GooseSessionNotification is a _goose/unstable/session/update
// notification; Update is tagged by sessionUpdate like ACP's own.
type GooseSessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// GooseUpdateUsage is the sessionUpdate tag of GooseUsageUpdate.
const GooseUpdateUsage = "usage_update"

// GooseUsageUpdate is goose's usage_update. Used is the last model
// request's tokens (the context in use); the accumulated figures sum every
// model request of the session, subagents included, and are kept in
// goose's session store, so they continue across session/load.
type GooseUsageUpdate struct {
	SessionUpdate           string   `json:"sessionUpdate"`
	Used                    int64    `json:"used"`
	ContextLimit            int64    `json:"contextLimit"`
	AccumulatedInputTokens  int64    `json:"accumulatedInputTokens"`
	AccumulatedOutputTokens int64    `json:"accumulatedOutputTokens"`
	AccumulatedCost         *float64 `json:"accumulatedCost,omitempty"`
}

type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// Update is the union of the session/update payloads Switchworks reads.
type Update struct {
	SessionUpdate string `json:"sessionUpdate"`

	// message and thought chunks
	Content   json.RawMessage `json:"content,omitempty"` // ContentBlock, or []ToolCallContent on tool calls
	MessageID string          `json:"messageId,omitempty"`

	// tool_call / tool_call_update
	ToolCallFields

	// plan
	Entries []PlanEntry `json:"entries,omitempty"`

	// current_mode_update
	CurrentModeID string `json:"currentModeId,omitempty"`

	// usage_update
	Used *int64 `json:"used,omitempty"`
	Size *int64 `json:"size,omitempty"`
	Cost *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost,omitempty"`

	// notice
	Severity    string `json:"severity,omitempty"`
	Description string `json:"description,omitempty"`

	// session_info_update (title is shared with tool calls) and the
	// agent-specific _meta.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// ToolCallFields are the fields of ToolCall and ToolCallUpdate. On updates
// every field but the id is optional; nil means unchanged.
type ToolCallFields struct {
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      *string         `json:"title,omitempty"`
	Name       *string         `json:"name,omitempty"`
	Kind       *string         `json:"kind,omitempty"`
	Status     *string         `json:"status,omitempty"`
	Locations  []Location      `json:"locations,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
}

type Location struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// Tool kinds.
const (
	KindRead       = "read"
	KindEdit       = "edit"
	KindDelete     = "delete"
	KindMove       = "move"
	KindSearch     = "search"
	KindExecute    = "execute"
	KindThink      = "think"
	KindFetch      = "fetch"
	KindSwitchMode = "switch_mode"
	KindOther      = "other"
)

// Tool call statuses.
const (
	ToolPending    = "pending"
	ToolInProgress = "in_progress"
	ToolCompleted  = "completed"
	ToolFailed     = "failed"
)

// ToolCallContent is one entry of a tool call's content.
type ToolCallContent struct {
	Type       string        `json:"type"` // content | diff | terminal
	Content    *ContentBlock `json:"content,omitempty"`
	Path       string        `json:"path,omitempty"`
	OldText    *string       `json:"oldText,omitempty"`
	NewText    string        `json:"newText,omitempty"`
	TerminalID string        `json:"terminalId,omitempty"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// Text returns the text of a message chunk's content block.
func (u *Update) Text() string {
	var b ContentBlock
	if json.Unmarshal(u.Content, &b) != nil {
		return ""
	}
	switch b.Type {
	case "text":
		return b.Text
	case "resource_link":
		return b.URI
	}
	return ""
}

// ToolContent decodes a tool call's content list.
func (u *Update) ToolContent() []ToolCallContent {
	var cs []ToolCallContent
	_ = json.Unmarshal(u.Content, &cs)
	return cs
}

// ToolText joins the text of a tool call's content blocks.
func ToolText(cs []ToolCallContent) string {
	var out []string
	for _, c := range cs {
		if c.Type == "content" && c.Content != nil && c.Content.Type == "text" {
			out = append(out, c.Content.Text)
		}
	}
	return strings.Join(out, "\n")
}

// --- permissions ---

// Permission option kinds.
const (
	AllowOnce    = "allow_once"
	AllowAlways  = "allow_always"
	RejectOnce   = "reject_once"
	RejectAlways = "reject_always"
)

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// ToolCallUpdate is a tool call as embedded in a permission request.
type ToolCallUpdate struct {
	ToolCallFields
	Content json.RawMessage `json:"content,omitempty"`
}

// ToolContent decodes the embedded tool call's content list (nil if none).
func (t ToolCallUpdate) ToolContent() []ToolCallContent {
	if len(t.Content) == 0 {
		return nil
	}
	var cs []ToolCallContent
	_ = json.Unmarshal(t.Content, &cs)
	return cs
}

type RequestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionOutcome is "selected" (with OptionID) or "cancelled".
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type RequestPermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}
