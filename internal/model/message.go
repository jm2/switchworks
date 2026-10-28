// SPDX-License-Identifier: GPL-3.0-or-later

package model

import "time"

// Message kinds.
const (
	MsgNotify = "notify" // fire and forget
	MsgAsk    = "ask"    // expects a fleet_reply
	MsgReply  = "reply"  // answers an ask (ReplyTo)
	MsgDone   = "done"   // completion notice from a spawned session to its parent
	MsgSystem = "system" // Switchworks's own notices (expiry, failure)
	// MsgBroadcast is only a request kind: it expands into one notify per
	// matching session.
	MsgBroadcast = "broadcast"
)

// Message states.
const (
	MsgPending   = "pending"   // waiting for the target's node
	MsgDelivered = "delivered" // accepted into the target's queue (or the human inbox)
	// MsgSeen: the turn that carried it finished (SeenAt, SeenTurn). An
	// ask can still be answered; answered wins over seen.
	MsgSeen      = "seen"
	MsgAnswered  = "answered" // an ask that has been replied to
	MsgExpired   = "expired"
	MsgFailed    = "failed"
	MsgCancelled = "cancelled" // taken out of the queue before delivery
	// MsgSuperseded: replaced, before delivery, by a newer message with the
	// same Supersedes key from the same sender to the same session
	// (SupersededBy).
	MsgSuperseded = "superseded"
	// MsgDismissed: an ask a person cleared from the Inbox without an
	// answer (DismissedBy, DismissedAt).
	MsgDismissed = "dismissed"
)

// Delivery modes.
const (
	ModeQueue     = "queue"
	ModeSteer     = "steer"
	ModeInterrupt = "interrupt"
	// ModeAuto steers when the target has a running turn its harness can
	// steer (and the session accepts steering), and queues otherwise. Nodes
	// before 0.1.5 take it as queue.
	ModeAuto = "auto"
)

// SystemSender is the From of Switchworks's own notices.
const SystemSender = "swk"

// Message is one routed message.
type Message struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	From        string `json:"from"`                   // session path, "user:<name>" or "swk"
	FromSession string `json:"from_session,omitempty"` // sender session id
	// Via names the credential a person sent it with ("admin" or
	// "token:<name>"), and Owner says whether that is the owner's; the
	// coordinator sets both from the request.
	Via   string `json:"via,omitempty"`
	Owner bool   `json:"owner,omitempty"`
	// ViaToken is the id of the API token it was sent with (coordinator
	// only): who may promote or cancel it is checked by id, since a later
	// token may take a revoked one's name. Older records have none.
	ViaToken  string `json:"via_token,omitempty"`
	To        string `json:"to"`                   // the address as given
	ToSession string `json:"to_session,omitempty"` // resolved target session id
	ToPath    string `json:"to_path,omitempty"`
	ToUser    string `json:"to_user,omitempty"` // "user:<name>" for the human inbox
	Text      string `json:"text"`
	// Attachments are images sent with the message. A stored record holds
	// their metadata only (no Data); the coordinator keeps the bytes in its
	// attachment store while they are needed (see docs/messaging.md).
	Attachments []Attachment `json:"attachments,omitempty"`
	Mode        string       `json:"mode,omitempty"`
	ReplyTo     string       `json:"reply_to,omitempty"` // ask id, for replies
	ReplyID     string       `json:"reply_id,omitempty"` // reply id, on answered asks
	Group       string       `json:"group,omitempty"`    // shared by the messages of one broadcast
	Hops        int          `json:"hops"`
	State       string       `json:"state"`
	Error       string       `json:"error,omitempty"`
	Inline      bool         `json:"inline,omitempty"` // reply (or an ask's dismissal) handed straight to a waiting fleet_ask
	// Supersedes is the sender's key for what the message reports (e.g.
	// "status:gate5"): a newer message with the same key from the same
	// sender to the same session replaces it while it is still pending or
	// queued, and SupersededBy is then that message's id.
	Supersedes   string `json:"supersedes,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// Files are text files sent with the message, kept like Attachments
	// (metadata here, bytes in the attachment store). The receiving node
	// writes them to disk and names their paths in the message.
	Files []Attachment `json:"files,omitempty"`
	// HoldUntil (asks): the asker collects the reply itself until then, so
	// a reply that arrives earlier is held rather than delivered as a turn.
	HoldUntil time.Time `json:"hold_until,omitempty"`
	// NotBefore (held replies, reminders, dismissal notices): not
	// delivered as a message before this.
	NotBefore time.Time `json:"not_before,omitempty"`
	// Reminder (system messages a session set with fleet_remind) is the
	// reminder's id: one pending reminder per id per session.
	Reminder  string    `json:"reminder,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// DeliveredAt is when the message went into its target's input: at
	// once for a steer (and the human inbox), when its turn starts for one
	// that waited in the session's queue (zero until then).
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
	// SeenAt is when the turn that carried it (SeenTurn) finished, and
	// SeenItem that turn's last agent message (an item id in the session's
	// events), the reply a receipt links to.
	SeenAt   time.Time `json:"seen_at,omitzero"`
	SeenTurn string    `json:"seen_turn,omitempty"`
	SeenItem string    `json:"seen_item,omitempty"`
	// ReadAt (messages to people) is when a person marked it read in the
	// Inbox, and ReadBy who: it no longer counts as new there. A read ask
	// can still be answered or dismissed.
	ReadAt time.Time `json:"read_at,omitzero"`
	ReadBy string    `json:"read_by,omitempty"`
	// DismissedAt and DismissedBy (dismissed asks) say when and by whom.
	// NoticeID is the system notice that tells the asker, sent once the
	// undo window has passed; none when a waiting asker was told at once
	// (Inline) or the asker is a person.
	DismissedAt time.Time `json:"dismissed_at,omitzero"`
	DismissedBy string    `json:"dismissed_by,omitempty"`
	NoticeID    string    `json:"notice_id,omitempty"`
}

// Terminal reports whether the message's delivery is over (asks can still
// move from delivered or seen to answered; another delivered message can
// still be marked seen, which only adds its receipt).
func (m Message) Terminal() bool {
	switch m.State {
	case MsgExpired, MsgFailed, MsgAnswered, MsgCancelled, MsgSuperseded, MsgDismissed:
		return true
	case MsgDelivered, MsgSeen:
		return m.Kind != MsgAsk
	}
	return false
}

// Accepted reports whether the target's node has taken the message and it
// has not yet finished: delivered, or seen but (an ask) not answered.
func (m Message) Accepted() bool {
	return m.State == MsgDelivered || m.State == MsgSeen
}

// Entered reports whether the message went into its target's input (it has
// a delivered_at or a receipt) and was not dropped there: one still in the
// session's queue, or cancelled, superseded, expired or failed, has not
// reached it.
func (m Message) Entered() bool {
	switch m.State {
	case MsgDelivered, MsgSeen, MsgAnswered, MsgDismissed:
		return !m.DeliveredAt.IsZero() || !m.SeenAt.IsZero()
	}
	return false
}

// Attachment is an image sent with a message or a spawn prompt. Data is
// base64 in JSON. Records that outlive delivery (events, stored messages)
// keep only the metadata: name, MIME type, size and SHA-256.
type Attachment struct {
	Name   string `json:"name,omitempty"`   // display name only; never used as a path
	MIME   string `json:"mime,omitempty"`   // sniffed from Data, never taken from the sender
	Size   int64  `json:"size,omitempty"`   // bytes
	SHA256 string `json:"sha256,omitempty"` // hex
	Data   []byte `json:"data,omitempty"`
}

// MetaOnly returns the attachments without their data.
func MetaOnly(atts []Attachment) []Attachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]Attachment, len(atts))
	for i, a := range atts {
		a.Data = nil
		out[i] = a
	}
	return out
}

// KindSessionSettled is emitted by a node when a session has finished all
// its work: the last turn completed and its message queue is empty.
const KindSessionSettled = "session.settled"

// Settled is the payload of session.settled.
type Settled struct {
	TurnID      string `json:"turn_id,omitempty"`
	TurnStatus  string `json:"turn_status,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// KindMessageSeen is emitted by a node (from 0.1.5) when the turn that
// carried a message, as a new turn or a steer, has completed.
const KindMessageSeen = "message.seen"

// MessageSeen is the payload of message.seen: the turn, and the item id of
// its last agent message (the reply), if it had one.
type MessageSeen struct {
	MessageID string `json:"message_id"`
	TurnID    string `json:"turn_id"`
	ItemID    string `json:"item_id,omitempty"`
}

// KindSessionQueue is emitted by a node when the number of messages queued
// for a session changes.
const KindSessionQueue = "session.queue"

// SessionQueue is the payload of session.queue.
type SessionQueue struct {
	Queued int `json:"queued"`
}
