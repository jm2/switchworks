// SPDX-License-Identifier: GPL-3.0-or-later

package model

import "time"

// Message kinds.
const (
	MsgNotify = "notify" // fire and forget
	MsgAsk    = "ask"    // expects a fleet_reply
	MsgReply  = "reply"  // answers an ask (ReplyTo)
	MsgDone   = "done"   // completion notice from a spawned session to its parent
	MsgSystem = "system" // Switchyard's own notices (expiry, failure)
	// MsgBroadcast is only a request kind: it expands into one notify per
	// matching session.
	MsgBroadcast = "broadcast"
)

// Message states.
const (
	MsgPending   = "pending"   // waiting for the target's node
	MsgDelivered = "delivered" // accepted into the target's queue (or the human inbox)
	MsgAnswered  = "answered"  // an ask that has been replied to
	MsgExpired   = "expired"
	MsgFailed    = "failed"
)

// Delivery modes.
const (
	ModeQueue     = "queue"
	ModeSteer     = "steer"
	ModeInterrupt = "interrupt"
)

// SystemSender is the From of Switchyard's own notices.
const SystemSender = "swyd"

// Message is one routed message.
type Message struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	From        string `json:"from"`                   // session path, "user:<name>" or "swyd"
	FromSession string `json:"from_session,omitempty"` // sender session id
	To          string `json:"to"`                     // the address as given
	ToSession   string `json:"to_session,omitempty"`   // resolved target session id
	ToPath      string `json:"to_path,omitempty"`
	ToUser      string `json:"to_user,omitempty"` // "user:<name>" for the human inbox
	Text        string `json:"text"`
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
	Inline      bool         `json:"inline,omitempty"` // reply handed straight to a waiting fleet_ask
	// HoldUntil (asks): the asker collects the reply itself until then, so
	// a reply that arrives earlier is held rather than delivered as a turn.
	HoldUntil time.Time `json:"hold_until,omitempty"`
	// NotBefore (held replies): not delivered as a message before this.
	NotBefore   time.Time `json:"not_before,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// Terminal reports whether the message will not change state again (asks
// can still move from delivered to answered).
func (m Message) Terminal() bool {
	switch m.State {
	case MsgExpired, MsgFailed, MsgAnswered:
		return true
	case MsgDelivered:
		return m.Kind != MsgAsk
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
