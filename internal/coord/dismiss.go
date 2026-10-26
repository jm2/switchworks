// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
)

// dismissUndo is how long the notice that tells an asker its ask was
// dismissed is held back, so that the person can undo the dismissal first
// (the web UI offers Undo for a little less).
const dismissUndo = 8 * time.Second

// Errors from undismiss: the ask is not dismissed, or its asker already
// knows it was.
var (
	errNotDismissed = errors.New("the ask is not dismissed")
	errTold         = errors.New("the asker has already been told it was dismissed; it can no longer be undone")
)

// personWho names a person and the credential they used, as a dismissal or
// a read mark records them.
func personWho(by sender) string {
	if by.via == "" {
		return by.addr()
	}
	return by.addr() + " (" + by.via + ")"
}

// dismiss ends an ask to a person without an answer. A fleet_ask or swk
// ask waiting on it returns at once, failed with "dismissed without an
// answer" (and the ask is marked inline). An asker whose wait has ended is
// sent a system notice, held back for dismissUndo, or until the ask's hold
// window closes (its swk mcp may still collect the dismissal with
// fleet_inbox(ticket), and then needs no notice), so that undismiss can
// take it back before it goes.
func (r *router) dismiss(id string, by sender) (model.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || cur.Kind != model.MsgAsk || cur.ToUser == "" {
		return cur, fmt.Errorf("%w: no ask to a person with id %q", store.ErrNotFound, id)
	}
	if cur.ReplyID != "" || (cur.State != model.MsgPending && !cur.Accepted()) {
		return cur, fmt.Errorf("%w: it is %s", ErrAskClosed, cur.State)
	}
	now := time.Now().UTC()
	who := personWho(by)
	cur.State, cur.Error = model.MsgDismissed, "dismissed without an answer by "+who
	cur.DismissedAt, cur.DismissedBy = now, who
	ws := r.waiters[id]
	delete(r.waiters, id)
	cur.Inline = len(ws) > 0
	if sess, ok := r.s.store.Session(cur.FromSession); !cur.Inline && cur.FromSession != "" && ok && live(sess) {
		// Built as notifySender builds its notices, but left for the loop
		// to deliver after NotBefore.
		n := model.Message{ID: ulid.New(), Kind: model.MsgSystem, From: model.SystemSender, To: sess.Path,
			ToSession: sess.ID, ToPath: sess.Path, Mode: r.steerMode(sender{}, sess, model.ModeAuto), Hops: cur.Hops, State: model.MsgPending,
			Text:      fmt.Sprintf("Your ask %s to %s was dismissed by %s without an answer; no answer will come.", cur.ID, cur.To, who),
			CreatedAt: now, ExpiresAt: now.Add(time.Duration(r.guards.DefaultTTLSeconds) * time.Second),
			NotBefore: now.Add(dismissUndo)}
		if n.NotBefore.Before(cur.HoldUntil) {
			n.NotBefore = cur.HoldUntil
		}
		if r.put(n) == nil {
			cur.NoticeID = n.ID
		}
	}
	if err := r.put(cur); err != nil {
		return cur, err
	}
	for _, w := range ws {
		w <- cur // the ask itself, dismissed
	}
	return cur, nil
}

// undismiss reopens a dismissed ask while its asker has not been told:
// no waiting asker was woken, and its notice has not gone out (it is
// withdrawn, marked cancelled).
func (r *router) undismiss(id string) (model.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || cur.Kind != model.MsgAsk || cur.ToUser == "" {
		return cur, fmt.Errorf("%w: no ask to a person with id %q", store.ErrNotFound, id)
	}
	if cur.State != model.MsgDismissed {
		return cur, fmt.Errorf("%w: it is %s", errNotDismissed, cur.State)
	}
	if cur.Inline {
		return cur, errTold
	}
	if cur.NoticeID != "" {
		n, ok := r.s.store.Message(cur.NoticeID)
		if ok && (n.State != model.MsgPending || r.inflight[n.ID]) {
			return cur, errTold
		}
		if ok {
			n.State, n.Error = model.MsgCancelled, "withdrawn: the dismissal was undone"
			_ = r.put(n)
		}
	}
	cur.State = model.MsgDelivered
	if cur.DeliveredAt.IsZero() {
		cur.State = model.MsgPending
	}
	cur.Error, cur.DismissedAt, cur.DismissedBy, cur.NoticeID = "", time.Time{}, "", ""
	if err := r.put(cur); err != nil {
		return cur, err
	}
	return cur, nil
}

// dismissalSeen records that an asker learned of its ask's dismissal by
// checking the ask itself (a ticket, or a wait that found it dismissed):
// it can no longer be undone, and a notice not yet sent is not needed.
func (r *router) dismissalSeen(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || cur.State != model.MsgDismissed || cur.Inline {
		return
	}
	cur.Inline = true
	if n, ok := r.s.store.Message(cur.NoticeID); ok && n.State == model.MsgPending && !r.inflight[n.ID] {
		n.State, n.Error = model.MsgCancelled, "not needed: the asker saw the dismissal with its ask"
		_ = r.put(n)
	}
	_ = r.put(cur)
}

// markInboxRead marks a message to a person read: it leaves the Inbox's new
// items and its badge count. It is kept on the message, so it holds for
// every browser and client; an ask stays open.
func (r *router) markInboxRead(id string, by sender) (model.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.s.store.Message(id)
	if !ok || cur.ToUser == "" {
		return cur, fmt.Errorf("%w: no message to a person with id %q", store.ErrNotFound, id)
	}
	if !cur.ReadAt.IsZero() {
		return cur, nil
	}
	cur.ReadAt, cur.ReadBy = time.Now().UTC(), personWho(by)
	if err := r.put(cur); err != nil {
		return cur, err
	}
	return cur, nil
}

// inboxStatus is the HTTP status for an error from dismiss, undismiss or
// markInboxRead.
func inboxStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404
	case errors.Is(err, ErrAskClosed), errors.Is(err, errNotDismissed), errors.Is(err, errTold):
		return 409
	}
	return 400
}

// inboxAction serves the Inbox's actions on one message: {from}, which may
// be left out (an empty body), like a reply's.
func (s *Server) inboxAction(fn func(id string, by sender) (model.Message, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			From string `json:"from,omitempty"`
		}
		if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, err)
			return
		}
		from, err := s.userSender(r.Context(), body.From)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		m, err := fn(r.PathValue("id"), from)
		if err != nil {
			writeErr(w, inboxStatus(err), err)
			return
		}
		writeJSON(w, 200, m)
	}
}

// handleDismiss ends an ask to a person without an answer.
func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	s.inboxAction(s.router.dismiss)(w, r)
}

// handleUndismiss undoes a dismissal the asker has not been told of.
func (s *Server) handleUndismiss(w http.ResponseWriter, r *http.Request) {
	s.inboxAction(func(id string, _ sender) (model.Message, error) { return s.router.undismiss(id) })(w, r)
}

// handleMarkRead marks a message to a person read.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	s.inboxAction(s.router.markInboxRead)(w, r)
}
