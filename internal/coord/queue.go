// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/wire"
)

// A session's queue lives on its node: these endpoints ask the node, which
// lists it, delivers one message now (promote) or drops one (cancel).

// errNodeTooOld is the answer when a session's node predates the queue
// ops.
var errNodeTooOld = errors.New("runs an older swk node without queue control; upgrade it")

// queueCommand sends a queue op to a session's node.
func (s *Server) queueCommand(ctx context.Context, sess model.Session, cmd wire.Cmd) (wire.Result, error) {
	res, err := s.SessionCommand(ctx, sess.ID, cmd)
	var rej *errRejected
	if errors.As(err, &rej) && strings.HasPrefix(rej.msg, "unknown op") {
		return res, fmt.Errorf("host %s %w", sess.Host, errNodeTooOld)
	}
	return res, err
}

// queueError writes the answer for a failed queue op: 409 when the node
// refused it (the message is not queued, or is being delivered) or is too
// old, as commandError otherwise.
func (s *Server) queueError(w http.ResponseWriter, err error) bool {
	var rej *errRejected
	if errors.As(err, &rej) || errors.Is(err, errNodeTooOld) {
		writeErr(w, 409, err)
		return true
	}
	return s.commandError(w, err)
}

// handleQueue lists a session's queued messages, in the order they will
// be delivered, with what the coordinator knows of each.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	res, err := s.queueCommand(r.Context(), sess, wire.Cmd{Op: wire.OpQueue})
	if s.queueError(w, err) {
		return
	}
	var qr wire.QueueResult
	if err := json.Unmarshal(res.Data, &qr); err != nil {
		writeErr(w, 502, fmt.Errorf("node %s: bad queue: %w", sess.Host, err))
		return
	}
	out := apiclient.Queue{Session: sess.ID, Messages: []apiclient.QueuedMessage{}}
	for _, e := range qr.Messages {
		q := apiclient.QueuedMessage{QueueEntry: e}
		// The node's word on who sent it is what the agent will see; the
		// coordinator's record adds the text, credential and times.
		if m, ok := s.store.Message(e.ID); ok && m.ToSession == sess.ID {
			q.Text, q.Via, q.CreatedAt = clipText(m.Text, 2000), m.Via, m.CreatedAt
		}
		out.Messages = append(out.Messages, q)
	}
	writeJSON(w, 200, out)
}

// queuedMessage loads a message for promote or cancel, and checks that the
// request may act on it: its sender (the same credential), or an owner.
func (s *Server) queuedMessage(w http.ResponseWriter, r *http.Request) (model.Message, bool) {
	m, ok := s.store.Message(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return m, false
	}
	via, owner, _ := s.credential(r.Context())
	if !owner && !(m.FromSession == "" && strings.HasPrefix(m.From, "user:") && m.Via != "" && m.Via == via) {
		writeErr(w, 403, errors.New("only the message's sender or an owner may change its delivery"))
		return m, false
	}
	if m.ToSession == "" {
		writeErr(w, 409, errors.New("only messages to sessions are queued"))
		return m, false
	}
	return m, true
}

// handlePromote delivers a queued message now: steered into the running
// turn, or with interrupt. One the coordinator still holds (its host is
// offline) takes that mode when it is delivered.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	switch body.Mode {
	case "":
		body.Mode = model.ModeSteer
	case model.ModeSteer, model.ModeInterrupt:
	default:
		writeErr(w, 400, fmt.Errorf("unknown mode %q (steer, interrupt)", body.Mode))
		return
	}
	m, ok := s.queuedMessage(w, r)
	if !ok {
		return
	}
	if m.State == model.MsgPending {
		out, changed := s.router.settle(m.ID, func(cur *model.Message) { cur.Mode = body.Mode })
		if !changed {
			out, _ = s.store.Message(m.ID)
		}
		writeJSON(w, 202, apiclient.SendResult{MessageID: out.ID, State: out.State, Mode: out.Mode})
		return
	}
	if m.State != model.MsgDelivered {
		writeErr(w, 409, fmt.Errorf("message %s is %s", m.ID, m.State))
		return
	}
	sess, ok := s.store.Session(m.ToSession)
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	res, err := s.queueCommand(r.Context(), sess, wire.Cmd{Op: wire.OpPromote, Queued: &wire.QueuedCmd{MessageID: m.ID, Mode: body.Mode}})
	if s.queueError(w, err) {
		return
	}
	var dr wire.DeliverResult
	_ = json.Unmarshal(res.Data, &dr)
	out := s.router.promoted(m.ID, dr.Mode)
	writeJSON(w, 202, apiclient.SendResult{MessageID: out.ID, State: out.State, Mode: out.Mode})
}

// handleCancel takes a message out of its session's queue, so it is never
// delivered, and marks it cancelled.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.queuedMessage(w, r)
	if !ok {
		return
	}
	who := "an owner"
	if via, owner, _ := s.credential(r.Context()); !owner {
		who = m.From + " (" + via + ")"
	}
	switch m.State {
	case model.MsgPending:
		if err := s.router.cancelPending(m.ID, who); err != nil {
			writeErr(w, 409, err)
			return
		}
	case model.MsgDelivered:
		sess, ok := s.store.Session(m.ToSession)
		if !ok {
			writeErr(w, 404, store.ErrNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if _, err := s.queueCommand(ctx, sess, wire.Cmd{Op: wire.OpCancel, Queued: &wire.QueuedCmd{MessageID: m.ID}}); s.queueError(w, err) {
			return
		}
		s.router.cancelled(m.ID, who)
	default:
		writeErr(w, 409, fmt.Errorf("message %s is %s", m.ID, m.State))
		return
	}
	out, _ := s.store.Message(m.ID)
	writeJSON(w, 200, apiclient.SendResult{MessageID: out.ID, State: out.State, Mode: out.Mode})
}
