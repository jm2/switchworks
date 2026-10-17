// SPDX-License-Identifier: GPL-3.0-or-later

package codex

import "encoding/json"

// SchemaVersion is the Codex version whose app-server JSON schema is committed
// under schema/codex/<version>/ and checked by the contract test. These types
// cover only the subset of the protocol Switchworks uses; unknown fields are
// ignored on decode, and every field we send must exist in the schema
// (enforced by TestContractAgainstSchema).
const SchemaVersion = "0.160.0"

// Methods (client -> server requests).
const (
	MethodInitialize        = "initialize"
	MethodThreadStart       = "thread/start"
	MethodThreadResume      = "thread/resume"
	MethodThreadSetName     = "thread/name/set"
	MethodThreadList        = "thread/list"
	MethodThreadRead        = "thread/read"
	MethodThreadUnsubscribe = "thread/unsubscribe"
	MethodTurnStart         = "turn/start"
	MethodTurnSteer         = "turn/steer"
	MethodTurnInterrupt     = "turn/interrupt"

	// Background terminals: commands app-server keeps running after the
	// turn that started them (experimental API).
	MethodBackgroundTerminalsList      = "thread/backgroundTerminals/list"
	MethodBackgroundTerminalsTerminate = "thread/backgroundTerminals/terminate"
	MethodBackgroundTerminalsClean     = "thread/backgroundTerminals/clean"
)

// Client -> server notifications.
const NotifyInitialized = "initialized"

// Server -> client notifications we handle.
const (
	NoteThreadStarted       = "thread/started"
	NoteThreadStatusChanged = "thread/status/changed"
	NoteThreadClosed        = "thread/closed"
	NoteTurnStarted         = "turn/started"
	NoteTurnCompleted       = "turn/completed"
	NoteItemStarted         = "item/started"
	NoteItemCompleted       = "item/completed"
	NoteAgentMessageDelta   = "item/agentMessage/delta"
	NoteTurnDiffUpdated     = "turn/diff/updated"
	NoteTokenUsageUpdated   = "thread/tokenUsage/updated"
	NoteError               = "error"
	NoteRequestResolved     = "serverRequest/resolved"
)

// Server -> client requests we handle.
const (
	RequestCommandApproval    = "item/commandExecution/requestApproval"
	RequestFileChangeApproval = "item/fileChange/requestApproval"
)

// ClientInfo identifies this client to app-server.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeCapabilities are negotiated once per connection.
type InitializeCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi,omitempty"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo              `json:"clientInfo"`
	Capabilities *InitializeCapabilities `json:"capabilities,omitempty"`
}

type InitializeResponse struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOs     string `json:"platformOs"`
}

// ThreadStatus is a tagged union: notLoaded | idle | systemError | active.
type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

// Thread is the subset of Codex's Thread object we read.
type Thread struct {
	ID             string        `json:"id"`
	SessionID      string        `json:"sessionId"`
	Preview        string        `json:"preview"`
	ModelProvider  string        `json:"modelProvider"`
	Model          *string       `json:"model"`
	Cwd            string        `json:"cwd"`
	Name           *string       `json:"name"`
	ParentThreadID *string       `json:"parentThreadId"`
	Path           *string       `json:"path"`
	Ephemeral      bool          `json:"ephemeral"`
	CreatedAt      int64         `json:"createdAt"`
	Status         *ThreadStatus `json:"status"`
}

type ThreadStartParams struct {
	Cwd                   string         `json:"cwd,omitempty"`
	Model                 string         `json:"model,omitempty"`
	ModelProvider         string         `json:"modelProvider,omitempty"`
	ApprovalPolicy        string         `json:"approvalPolicy,omitempty"`
	Sandbox               string         `json:"sandbox,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions,omitempty"`
	ServiceName           string         `json:"serviceName,omitempty"`
	Config                map[string]any `json:"config,omitempty"`
}

// ThreadStartResponse and ThreadResumeResponse share this shape.
type ThreadStartResponse struct {
	Thread          Thread          `json:"thread"`
	Model           string          `json:"model"`
	ModelProvider   string          `json:"modelProvider"`
	Cwd             string          `json:"cwd"`
	ApprovalPolicy  json.RawMessage `json:"approvalPolicy"`
	Sandbox         json.RawMessage `json:"sandbox"`
	ReasoningEffort *string         `json:"reasoningEffort"`
}

type ThreadResumeParams struct {
	ThreadID       string         `json:"threadId"`
	Cwd            string         `json:"cwd,omitempty"`
	Model          string         `json:"model,omitempty"`
	ModelProvider  string         `json:"modelProvider,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	Sandbox        string         `json:"sandbox,omitempty"`
	ExcludeTurns   bool           `json:"excludeTurns,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
}

type ThreadResumeResponse = ThreadStartResponse

type ThreadSetNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

type ThreadListParams struct {
	Cursor      *string  `json:"cursor,omitempty"`
	Limit       *int     `json:"limit,omitempty"`
	SourceKinds []string `json:"sourceKinds,omitempty"`
	Archived    *bool    `json:"archived,omitempty"`
}

type ThreadListResponse struct {
	Data       []Thread `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

type ThreadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns,omitempty"`
}

type ThreadReadResponse struct {
	Thread Thread `json:"thread"`
}

type ThreadUnsubscribeParams struct {
	ThreadID string `json:"threadId"`
}

type BackgroundTerminalsListParams struct {
	ThreadID string `json:"threadId"`
}

// BackgroundTerminal is one command still running for a thread. ItemID is
// the commandExecution item that started it.
type BackgroundTerminal struct {
	ProcessID string `json:"processId"`
	ItemID    string `json:"itemId"`
	Command   string `json:"command"`
}

type BackgroundTerminalsListResponse struct {
	Data []BackgroundTerminal `json:"data"`
}

type BackgroundTerminalsTerminateParams struct {
	ThreadID  string `json:"threadId"`
	ProcessID string `json:"processId"`
}

type BackgroundTerminalsCleanParams struct {
	ThreadID string `json:"threadId"`
}

type ThreadUnsubscribeResponse struct {
	Status string `json:"status"`
}

// UserInput: the text and localImage variants are used. localImage makes
// app-server read the image file itself (and encode it for the model), as
// the Codex TUI does for pasted images.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"` // text
	Path string `json:"path,omitempty"` // localImage
}

// MarshalJSON always writes text for the text variant (it is required).
func (u UserInput) MarshalJSON() ([]byte, error) {
	if u.Type == "text" {
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{u.Type, u.Text})
	}
	type plain UserInput
	return json.Marshal(plain(u))
}

// TextInput builds a text UserInput.
func TextInput(s string) []UserInput { return []UserInput{{Type: "text", Text: s}} }

// LocalImageInput builds a localImage UserInput.
func LocalImageInput(path string) UserInput { return UserInput{Type: "localImage", Path: path} }

type TurnStartParams struct {
	ThreadID            string      `json:"threadId"`
	Input               []UserInput `json:"input"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
	Cwd                 string      `json:"cwd,omitempty"`
	Model               string      `json:"model,omitempty"`
	Effort              string      `json:"effort,omitempty"`
}

type TurnError struct {
	Message           string          `json:"message"`
	AdditionalDetails *string         `json:"additionalDetails"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
}

// Turn statuses.
const (
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
	TurnInProgress  = "inProgress"
)

type Turn struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Error       *TurnError        `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMs  *int64            `json:"durationMs"`
	Items       []json.RawMessage `json:"items"`
}

type TurnStartResponse struct {
	Turn Turn `json:"turn"`
}

type TurnSteerParams struct {
	ThreadID            string      `json:"threadId"`
	ExpectedTurnID      string      `json:"expectedTurnId"`
	Input               []UserInput `json:"input"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
}

type TurnSteerResponse struct {
	TurnID string `json:"turnId"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// --- notifications ---

type ThreadStartedNotification struct {
	Thread Thread `json:"thread"`
}

type ThreadStatusChangedNotification struct {
	ThreadID string       `json:"threadId"`
	Status   ThreadStatus `json:"status"`
}

type ThreadClosedNotification struct {
	ThreadID string `json:"threadId"`
}

// TurnNotification is the shape of turn/started and turn/completed.
type TurnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

// ItemNotification is the shape of item/started and item/completed.
type ItemNotification struct {
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	Item          json.RawMessage `json:"item"`
	StartedAtMs   int64           `json:"startedAtMs"`
	CompletedAtMs int64           `json:"completedAtMs"`
}

// ItemHeader is the discriminator shared by every ThreadItem variant.
type ItemHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AgentMessageItem is the agentMessage ThreadItem variant.
type AgentMessageItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

type AgentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type TurnDiffUpdatedNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Diff     string `json:"diff"`
}

type TokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

type ThreadTokenUsage struct {
	Total              TokenUsageBreakdown `json:"total"`
	Last               TokenUsageBreakdown `json:"last"`
	ModelContextWindow *int64              `json:"modelContextWindow"`
}

type ThreadTokenUsageUpdatedNotification struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

type ErrorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     TurnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

type ServerRequestResolvedNotification struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

// --- server requests ---

type CommandExecutionRequestApprovalParams struct {
	ThreadID   string  `json:"threadId"`
	TurnID     string  `json:"turnId"`
	ItemID     string  `json:"itemId"`
	ApprovalID *string `json:"approvalId"`
	Command    *string `json:"command"`
	Cwd        *string `json:"cwd"`
	Reason     *string `json:"reason"`
	// AvailableDecisions is the ordered set of decisions this prompt allows
	// (experimental API). Entries are strings ("accept") or single-key
	// objects ({"acceptWithExecpolicyAmendment": ...}).
	AvailableDecisions []json.RawMessage `json:"availableDecisions"`
}

// DecisionNames flattens AvailableDecisions to their names: the string value,
// or the single key of an object-valued decision.
func DecisionNames(raw []json.RawMessage) []string {
	var out []string
	for _, r := range raw {
		var s string
		if json.Unmarshal(r, &s) == nil {
			out = append(out, s)
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(r, &m) == nil {
			for k := range m {
				out = append(out, k)
			}
		}
	}
	return out
}

type FileChangeRequestApprovalParams struct {
	ThreadID  string  `json:"threadId"`
	TurnID    string  `json:"turnId"`
	ItemID    string  `json:"itemId"`
	Reason    *string `json:"reason"`
	GrantRoot *string `json:"grantRoot"`
}

// ApprovalResponse answers both approval request kinds. Decision is one of
// "accept", "acceptForSession", "decline", "cancel" (the object-valued
// decisions are not used by Switchworks).
type ApprovalResponse struct {
	Decision string `json:"decision"`
}

// Approval decisions.
const (
	DecisionAccept           = "accept"
	DecisionAcceptForSession = "acceptForSession"
	DecisionDecline          = "decline"
	DecisionCancel           = "cancel"
)

// ThreadIDOf extracts the threadId field present on most notifications.
func ThreadIDOf(params json.RawMessage) string {
	var v struct {
		ThreadID string `json:"threadId"`
		Thread   *struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(params, &v) != nil {
		return ""
	}
	if v.ThreadID != "" {
		return v.ThreadID
	}
	if v.Thread != nil {
		return v.Thread.ID
	}
	return ""
}
