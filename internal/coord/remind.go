// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
)

// A reminder (fleet_remind) is a system message from Switchworks to the
// session that set it, stored pending with NotBefore at its due time: the
// router's loop delivers it then, like a dismissal notice, and like every
// pending message it survives a coordinator restart. Its Reminder field is
// the session's id for it; a new reminder with the id of a pending one
// supersedes that one. When the session stops or goes, its pending
// reminders expire.

// errNoReminder answers a cancel of a reminder that is not pending.
var errNoReminder = errors.New("no pending reminder")

// remind sets or cancels a reminder for the calling session.
func (r *router) remind(from sender, a fleet.RemindArgs) (fleet.RemindResult, error) {
	if from.sess == nil {
		return fleet.RemindResult{}, errors.New("only agent sessions set reminders")
	}
	sess := *from.sess
	if a.Cancel != "" {
		if a.InSeconds != 0 || a.Text != "" || a.ID != "" {
			return fleet.RemindResult{}, errors.New("cancel takes no other arguments")
		}
		m, err := r.cancelReminder(sess.ID, a.Cancel, "its session")
		if err != nil {
			return fleet.RemindResult{}, err
		}
		return fleet.RemindResult{ID: m.Reminder, MessageID: m.ID, Cancelled: true}, nil
	}
	id := strings.TrimSpace(a.ID)
	text := strings.TrimSpace(a.Text)
	switch {
	case a.InSeconds < fleet.MinRemindSeconds || a.InSeconds > fleet.MaxRemindSeconds:
		return fleet.RemindResult{}, fmt.Errorf("in_seconds must be from %d to %d", fleet.MinRemindSeconds, fleet.MaxRemindSeconds)
	case text == "":
		return fleet.RemindResult{}, errors.New("text is required")
	case len(text) > fleet.MaxRemindTextBytes || !utf8.ValidString(text):
		return fleet.RemindResult{}, fmt.Errorf("text must be UTF-8 of at most %d bytes", fleet.MaxRemindTextBytes)
	case !validReminderID(id):
		return fleet.RemindResult{}, fmt.Errorf("id must be printable UTF-8 of at most %d bytes", fleet.MaxRemindIDBytes)
	}
	delay := time.Duration(a.InSeconds) * time.Second
	hops := r.hopsFor(from)

	// One at a time per coordinator, so two calls with one id cannot both
	// stay pending, nor two new ones both pass the cap.
	r.remindMu.Lock()
	defer r.remindMu.Unlock()
	pending := r.reminders(sess.ID)
	var old *model.Message
	for i := range pending {
		if id != "" && pending[i].Reminder == id {
			old = &pending[i]
		}
	}
	if old == nil && len(pending) >= fleet.MaxReminders {
		return fleet.RemindResult{}, fmt.Errorf("this session has %d pending reminders, the most it may have; cancel one or reuse its id", len(pending))
	}
	now := time.Now().UTC()
	m := model.Message{ID: ulid.New(), Kind: model.MsgSystem, From: model.SystemSender, FromSession: sess.ID,
		To: sess.Path, ToSession: sess.ID, ToPath: sess.Path, Mode: r.steerMode(sender{}, sess, model.ModeAuto),
		Hops: hops, State: model.MsgPending, CreatedAt: now, NotBefore: now.Add(delay),
		ExpiresAt: now.Add(delay + time.Duration(r.guards.DefaultTTLSeconds)*time.Second)}
	if id == "" {
		id = m.ID
	}
	m.Reminder = id
	m.Text = fmt.Sprintf("Reminder %q, which you set %s ago with fleet_remind:\n%s", id, shortDuration(delay), text)
	if err := r.put(m); err != nil {
		return fleet.RemindResult{}, err
	}
	res := fleet.RemindResult{ID: id, MessageID: m.ID, DueAt: m.NotBefore}
	if old != nil {
		r.superseded(old.ID, m.ID, false)
		res.Replaced = old.ID
	}
	return res, nil
}

// shortDuration is d without trailing zero minutes and seconds: "10m",
// "1h", "1h30m".
func shortDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// validReminderID reports whether id may name a reminder: empty (one is
// made), or short printable text.
func validReminderID(id string) bool {
	if len(id) > fleet.MaxRemindIDBytes || !utf8.ValidString(id) {
		return false
	}
	for _, c := range id {
		if !unicode.IsPrint(c) {
			return false
		}
	}
	return true
}

// reminders are a session's pending reminders, soonest first.
func (r *router) reminders(sessionID string) []model.Message {
	var out []model.Message
	for _, m := range r.s.store.PendingMessages() {
		if m.Reminder != "" && m.Kind == model.MsgSystem && m.ToSession == sessionID && m.FromSession == sessionID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotBefore.Before(out[j].NotBefore) })
	return out
}

// cancelReminder cancels a session's pending reminder, named by its id or
// its message's.
func (r *router) cancelReminder(sessionID, ref, by string) (model.Message, error) {
	r.remindMu.Lock()
	defer r.remindMu.Unlock()
	for _, m := range r.reminders(sessionID) {
		if m.Reminder != ref && m.ID != ref {
			continue
		}
		if err := r.cancelPending(m.ID, by); err != nil {
			return m, err
		}
		out, _ := r.s.store.Message(m.ID)
		return out, nil
	}
	return model.Message{}, fmt.Errorf("%w %q", errNoReminder, ref)
}

// dropReminders expires the pending reminders of a session that has
// stopped or gone: no turn will take them. One being delivered at this
// moment is left to its delivery, which fails it.
func (r *router) dropReminders(sess model.Session, why string) {
	r.remindMu.Lock()
	defer r.remindMu.Unlock()
	for _, m := range r.reminders(sess.ID) {
		r.mu.Lock()
		if cur, ok := r.s.store.Message(m.ID); ok && cur.State == model.MsgPending && !r.inflight[m.ID] {
			cur.State, cur.Error = model.MsgExpired, why
			_ = r.put(cur)
		}
		r.mu.Unlock()
	}
}

// reminderInfo is a pending reminder as the API lists it.
func reminderInfo(m model.Message) fleet.Reminder {
	return fleet.Reminder{ID: m.Reminder, MessageID: m.ID, Text: clipText(m.Text, 2000), DueAt: m.NotBefore,
		CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt}
}

// handleReminders lists a session's pending reminders, soonest first.
func (s *Server) handleReminders(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	out := struct {
		Session   string           `json:"session"`
		Reminders []fleet.Reminder `json:"reminders"`
	}{Session: sess.ID, Reminders: []fleet.Reminder{}}
	for _, m := range s.router.reminders(sess.ID) {
		out.Reminders = append(out.Reminders, reminderInfo(m))
	}
	writeJSON(w, 200, out)
}

// handleCancelReminder cancels one of a session's pending reminders (by its
// id or its message id). Only an owner may: the session set it for itself.
func (s *Server) handleCancelReminder(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	if !s.isOwner(r.Context()) {
		writeErr(w, 403, errors.New("only an owner may cancel a session's reminders"))
		return
	}
	m, err := s.router.cancelReminder(sess.ID, r.PathValue("rid"), "an owner")
	switch {
	case errors.Is(err, errNoReminder):
		writeErr(w, 404, err)
		return
	case err != nil:
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, fleet.RemindResult{ID: m.Reminder, MessageID: m.ID, Cancelled: true})
}
