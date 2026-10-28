// SPDX-License-Identifier: GPL-3.0-or-later

// Package wire is the node <-> coordinator protocol: newline-delimited JSON
// envelopes over one mutually authenticated TLS connection per node.
//
// The design doc proposed gRPC; this build uses NDJSON over mTLS instead
// because it needs no code generation or third-party modules, matches the
// JSONL style of the harness protocols, and carries the same semantics: one
// long-lived bidirectional stream per node, initiated by the node.
//
// Flow:
//
//	node  -> hello   {host, os, arch, version, adapters, sessions}
//	coord -> welcome {acked: session -> last stored seq}
//	node  -> events  {events}            (any time; resend after reconnect)
//	coord -> ack     {acked}
//	coord -> cmd     {id, op, ...}        node -> result {re: id, ok, error, data}
//	either -> ping / pong                 (keepalive)
package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/model"
)

// ProtocolVersion is bumped on incompatible changes.
const ProtocolVersion = 1

// Envelope types.
const (
	THello   = "hello"
	TWelcome = "welcome"
	TEvents  = "events"
	TAck     = "ack"
	TCmd     = "cmd"
	TResult  = "result"
	TPing    = "ping"
	TPong    = "pong"
	TError   = "error"
)

// Envelope is one line on the wire.
type Envelope struct {
	Type string          `json:"t"`
	ID   string          `json:"id,omitempty"`
	Re   string          `json:"re,omitempty"`
	P    json.RawMessage `json:"p,omitempty"`
}

// SessionState is a node's summary of one of its sessions.
type SessionState struct {
	ID        string           `json:"id"`
	Path      string           `json:"path"`
	Harness   string           `json:"harness"`
	Status    string           `json:"status"`
	LastSeq   int64            `json:"last_seq"`
	NativeRef *model.NativeRef `json:"native_ref,omitempty"`
	// What the session runs with (sessions it spawns inherit them).
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
}

type Hello struct {
	Protocol int            `json:"protocol"`
	Host     string         `json:"host"`
	OS       string         `json:"os"`
	Arch     string         `json:"arch"`
	Version  string         `json:"version"`
	Adapters []string       `json:"adapters"`
	Sessions []SessionState `json:"sessions"`
	// Features lists what this node enforces, so the coordinator does not
	// rely on an older node for checks it would ignore.
	Features []string `json:"features,omitempty"`
	// Sandboxed lists the harnesses on this node that enforce a session's
	// sandbox setting (Codex, agy); ACP agents do not.
	Sandboxed []string `json:"sandboxed,omitempty"`
	// Images lists the harnesses on this node that can take image
	// attachments: Codex, and ACP agents (each session reports in its caps
	// whether its agent advertises image prompts). agy cannot. Older nodes
	// send none, and the coordinator then refuses images for them.
	Images []string `json:"images,omitempty"`
}

// FeatureCwdRoot: the node confines a spawn to SpawnSpec.CwdRoot.
const FeatureCwdRoot = "cwd_root"

// FeatureAgentSpawn: the node honors SpawnSpec.ByAgent and NotBelow and
// pins an agent-spawned session's resolved cwd.
const FeatureAgentSpawn = "agent_spawn"

// FeatureFiles: the node takes MessageCmd.Files, writing them where its
// sessions can read them. An older node would drop them unseen, so the
// coordinator sends files only to nodes that list this.
const FeatureFiles = "files"

type Welcome struct {
	Protocol int              `json:"protocol"`
	Acked    map[string]int64 `json:"acked"`
}

type Events struct {
	Events []model.Event `json:"events"`
}

type Ack struct {
	Acked map[string]int64 `json:"acked"`
}

// Command ops.
const (
	OpSpawn     = "spawn"
	OpDeliver   = "deliver"
	OpInterrupt = "interrupt"
	OpResolve   = "resolve"
	OpStop      = "stop"
	OpPolicy    = "policy"
	// Queue ops (nodes from 0.1.5; older ones answer "unknown op"): list a
	// session's queue, deliver one queued message now, or drop one.
	OpQueue   = "queue"
	OpPromote = "promote"
	OpCancel  = "cancel"
)

type Cmd struct {
	Op      string         `json:"op"`
	Session string         `json:"session"`
	Spawn   *SpawnCmd      `json:"spawn,omitempty"`
	Message *MessageCmd    `json:"message,omitempty"`
	Resolve *ResolveCmd    `json:"resolve,omitempty"`
	Policy  *PolicyCmd     `json:"policy,omitempty"`
	Queued  *QueuedCmd     `json:"queued,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// QueuedCmd names a queued message for OpPromote (with Mode steer or
// interrupt) and OpCancel.
type QueuedCmd struct {
	MessageID string `json:"message_id"`
	Mode      string `json:"mode,omitempty"`
	// SupersededBy (OpCancel): the newer message that replaces it, which
	// message.cancelled names. Older nodes ignore it.
	SupersededBy string `json:"superseded_by,omitempty"`
}

// QueueResult answers OpQueue: the session's queued messages, in the
// order they will be delivered.
type QueueResult struct {
	Messages []QueueEntry `json:"messages"`
}

// QueueEntry is one queued message. Position counts the messages ahead of
// it.
type QueueEntry struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Kind     string `json:"kind,omitempty"`
	Owner    bool   `json:"owner,omitempty"`
	Images   int    `json:"images,omitempty"`
	Position int    `json:"position"`
	// ExpiresAt is when the node drops it undelivered (nodes from 0.1.5,
	// for messages that came with one).
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// AtStep: it goes into the running turn at the turn's next step.
	AtStep bool `json:"at_step,omitempty"`
}

// SpawnCmd carries the coordinator's session record and the request.
type SpawnCmd struct {
	Session model.Session   `json:"session"`
	Spec    model.SpawnSpec `json:"spec"`
	// PromptVia and PromptOwner describe the credential of a person's
	// spawn, for its first prompt (see MessageCmd). Here, not in Spec, so a
	// client cannot set them.
	PromptVia   string `json:"prompt_via,omitempty"`
	PromptOwner bool   `json:"prompt_owner,omitempty"`
}

// SpawnResult answers OpSpawn.
type SpawnResult struct {
	NativeRef      *model.NativeRef `json:"native_ref,omitempty"`
	ApprovalPolicy string           `json:"approval_policy,omitempty"`
	Sandbox        string           `json:"sandbox,omitempty"`
	// Cwd is the directory the session runs in, after the node expanded a
	// leading "~".
	Cwd string `json:"cwd,omitempty"`
}

type MessageCmd struct {
	ID   string `json:"id"`
	From string `json:"from"`
	// Via and Owner: see model.Message. A message from a person reaches
	// the agent without a header only when Owner is set.
	Via     string `json:"via,omitempty"`
	Owner   bool   `json:"owner,omitempty"`
	Text    string `json:"text"`
	Mode    string `json:"mode,omitempty"` // queue (default) | auto | steer | interrupt; nodes before 0.1.5 queue auto
	Kind    string `json:"kind,omitempty"` // notify | ask | reply | done | system
	ReplyTo string `json:"reply_to,omitempty"`
	// ReplyToText (replies): the start of the ask it answers, so the
	// asker knows which question this is without looking it up.
	ReplyToText string `json:"reply_to_text,omitempty"`
	// Attachments carry the image bytes (at most attach.MaxTotalBytes, so
	// the envelope stays well under MaxLine).
	Attachments []model.Attachment `json:"attachments,omitempty"`
	// Files carry text files' bytes (at most attach.MaxFileTotalBytes), to
	// nodes with FeatureFiles only.
	Files []model.Attachment `json:"files,omitempty"`
	// ExpiresAt is the message's expiry (model.Message.ExpiresAt): a node
	// from 0.1.5 drops it from the session's queue undelivered once past it
	// (message.expired). Older nodes ignore it and keep it until delivered.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// DeliverResult is the data of a successful deliver (or promote) command.
type DeliverResult struct {
	Mode      string `json:"mode"`                // how it was delivered: queue, steer or interrupt
	Duplicate bool   `json:"duplicate,omitempty"` // already seen; nothing done
	// Expired: it expired while waiting to be steered in, and was dropped
	// undelivered (message.expired).
	Expired bool `json:"expired,omitempty"`
}

// Node -> coordinator requests, sent as TCmd with a NodeReq payload; the
// coordinator answers with TResult. They carry fleet operations made by
// agents through `swk mcp`, on behalf of one of the node's sessions.
const (
	ReqList   = "fleet.list"
	ReqSend   = "fleet.send"
	ReqAsk    = "fleet.ask"
	ReqReply  = "fleet.reply"
	ReqInbox  = "fleet.inbox"
	ReqSpawn  = "fleet.spawn"
	ReqStatus = "fleet.status"
	ReqStop   = "fleet.stop"
	// ReqRemind (from 0.1.7) sets or cancels a reminder for the caller.
	ReqRemind = "fleet.remind"
)

// NodeReq is a node-initiated request.
type NodeReq struct {
	Op      string          `json:"op"`
	Session string          `json:"session"` // calling session id (must belong to the node)
	Args    json.RawMessage `json:"args,omitempty"`
}

// PolicyCmd changes a running session's approval policy (OpPolicy).
type PolicyCmd struct {
	ApprovalPolicy string `json:"approval_policy"`
}

type ResolveCmd struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
}

type Result struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// MaxLine bounds one envelope (a large turn diff can be big).
const MaxLine = 32 << 20

// Conn frames envelopes over a stream. Send is safe for concurrent use;
// Recv must be called from one goroutine.
type Conn struct {
	rw  io.ReadWriteCloser
	br  *bufio.Reader
	wmu sync.Mutex
}

// NewConn wraps a stream.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw, br: bufio.NewReaderSize(rw, 1<<20)}
}

// Send writes one envelope with payload p (may be nil).
func (c *Conn) Send(typ, id, re string, p any) error {
	env := Envelope{Type: typ, ID: id, Re: re}
	if p != nil {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		env.P = b
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// A peer that stops reading must not block senders forever: the
	// connection fails instead, and is redialed.
	if d, ok := c.rw.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(WriteTimeout))
		defer d.SetWriteDeadline(time.Time{})
	}
	_, err = c.rw.Write(b)
	return err
}

// WriteTimeout bounds one Send.
const WriteTimeout = time.Minute

// ErrTooLong is returned for envelopes over MaxLine.
var ErrTooLong = errors.New("wire: envelope too long")

// Recv reads one envelope.
func (c *Conn) Recv() (Envelope, error) {
	var env Envelope
	var line []byte
	for {
		chunk, err := c.br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLine {
			return env, ErrTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return env, err
		}
		break
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return env, fmt.Errorf("wire: bad envelope: %w", err)
	}
	return env, nil
}

// Close closes the stream.
func (c *Conn) Close() error { return c.rw.Close() }

// Decode unmarshals an envelope payload.
func Decode[T any](env Envelope) (T, error) {
	var v T
	if len(env.P) == 0 {
		return v, nil
	}
	err := json.Unmarshal(env.P, &v)
	return v, err
}
