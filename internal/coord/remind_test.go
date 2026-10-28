// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/wire"
)

// deliveries is a fake node that takes every message, steering it in, and
// passes on what it was given.
func deliveries(t *testing.T, s *Server, host string) chan wire.MessageCmd {
	t.Helper()
	got := make(chan wire.MessageCmd, 16)
	fakeNode(t, s, host, func(cmd wire.Cmd) wire.Result {
		if cmd.Message != nil {
			got <- *cmd.Message
		}
		return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeSteer})}
	})
	return got
}

// due makes a pending reminder due now and wakes the router.
func due(t *testing.T, s *Server, id string) {
	t.Helper()
	m, ok := s.store.Message(id)
	if !ok {
		t.Fatalf("no message %s", id)
	}
	m.NotBefore = time.Now().Add(-time.Second)
	if err := s.store.PutMessage(m); err != nil {
		t.Fatal(err)
	}
	s.router.poke()
}

func waitState(t *testing.T, s *Server, id, want string) model.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, _ := s.store.Message(id)
		if m.State == want {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s is %s, want %s: %+v", id, m.State, want, m)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A reminder waits until it is due, then reaches its own session as a
// system message (auto: steered into a running turn, or a turn of its
// own).
func TestRemindDelivers(t *testing.T) {
	s := testServer(t, Guards{})
	got := deliveries(t, s, "devbox")
	a := addSession(t, s, "devbox", "a")
	r := s.router

	res, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 60, Text: "check CI on #123", ID: "ci-123"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "ci-123" || res.MessageID == "" || time.Until(res.DueAt) < 55*time.Second || res.Replaced != "" {
		t.Fatalf("result %+v", res)
	}
	m, _ := s.store.Message(res.MessageID)
	if m.Kind != model.MsgSystem || m.From != model.SystemSender || m.FromSession != a.ID || m.ToSession != a.ID ||
		m.State != model.MsgPending || m.Reminder != "ci-123" || !m.NotBefore.Equal(res.DueAt) || !m.ExpiresAt.After(m.NotBefore) {
		t.Fatalf("stored %+v", m)
	}
	// Not before it is due, though its host is online.
	r.poke()
	select {
	case c := <-got:
		t.Fatalf("delivered early: %+v", c)
	case <-time.After(2500 * time.Millisecond):
	}
	if msgState(s, m.ID) != model.MsgPending {
		t.Fatalf("state %s", msgState(s, m.ID))
	}
	// It is not one of the session's queued (held) messages either.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions/"+a.ID+"/queue", nil)
	req.SetPathValue("id", a.ID)
	s.handleQueue(w, req)
	if strings.Contains(w.Body.String(), m.ID) {
		t.Fatalf("queue lists the reminder: %s", w.Body)
	}

	due(t, s, m.ID)
	select {
	case c := <-got:
		if c.ID != m.ID || c.Kind != model.MsgSystem || c.From != model.SystemSender ||
			!strings.Contains(c.Text, `Reminder "ci-123"`) || !strings.Contains(c.Text, "1m ago") || !strings.HasSuffix(c.Text, "check CI on #123") {
			t.Fatalf("delivered %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not delivered once due")
	}
	waitState(t, s, m.ID, model.MsgDelivered)
	if len(r.reminders(a.ID)) != 0 {
		t.Fatal("a delivered reminder is still pending")
	}
}

// A reminder with the id of a pending one replaces it; one without an id
// gets its own; a session has at most MaxReminders.
func TestRemindReplaceByID(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "offbox", "a")
	b := addSession(t, s, "offbox", "b")
	r := s.router

	first, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 600, Text: "check CI", ID: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 300, Text: "check CI again", ID: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Replaced != first.MessageID || second.MessageID == first.MessageID {
		t.Fatalf("second %+v, first %+v", second, first)
	}
	if old, _ := s.store.Message(first.MessageID); old.State != model.MsgSuperseded || old.SupersededBy != second.MessageID {
		t.Fatalf("replaced %+v", old)
	}
	// The same id in another session is another reminder.
	if res, err := r.remind(from(b), fleet.RemindArgs{InSeconds: 600, Text: "x", ID: "ci"}); err != nil || res.Replaced != "" {
		t.Fatalf("other session: %+v %v", res, err)
	}
	anon, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 60, Text: "soon"})
	if err != nil || anon.ID != anon.MessageID {
		t.Fatalf("no id: %+v %v", anon, err)
	}
	pend := r.reminders(a.ID)
	if len(pend) != 2 || pend[0].ID != anon.MessageID || pend[1].ID != second.MessageID {
		t.Fatalf("pending %+v", pend)
	}

	for _, bad := range []fleet.RemindArgs{
		{InSeconds: 59, Text: "x"},
		{InSeconds: fleet.MaxRemindSeconds + 1, Text: "x"},
		{InSeconds: 60, Text: "  "},
		{InSeconds: 60, Text: strings.Repeat("x", fleet.MaxRemindTextBytes+1)},
		{InSeconds: 60, Text: "x", ID: "a\nb"},
		{InSeconds: 60, Text: "x", ID: strings.Repeat("k", fleet.MaxRemindIDBytes+1)},
		{InSeconds: 60, Cancel: "ci"},
	} {
		if _, err := r.remind(from(a), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := r.remind(sender{user: "user:jm"}, fleet.RemindArgs{InSeconds: 60, Text: "x"}); err == nil {
		t.Error("a person set a reminder")
	}

	for i := len(r.reminders(a.ID)); i < fleet.MaxReminders; i++ {
		if _, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 60, Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 60, Text: "one too many"}); err == nil || !strings.Contains(err.Error(), "pending reminders") {
		t.Fatalf("over the cap: %v", err)
	}
	// Replacing one is still allowed at the cap.
	if _, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 60, Text: "x", ID: "ci"}); err != nil {
		t.Fatalf("replace at the cap: %v", err)
	}
}

// fleet_remind {cancel} cancels a session's own reminder (by its id or its
// message id); an owner can cancel one through the API, and others cannot.
func TestRemindCancel(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "offbox", "a")
	b := addSession(t, s, "offbox", "b")
	r := s.router

	res, err := r.remind(from(a), fleet.RemindArgs{InSeconds: 600, Text: "check CI", ID: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.remind(from(b), fleet.RemindArgs{Cancel: "ci"}); err == nil {
		t.Fatal("another session cancelled it")
	}
	c, err := r.remind(from(a), fleet.RemindArgs{Cancel: "ci"})
	if err != nil || !c.Cancelled || c.MessageID != res.MessageID || c.ID != "ci" {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if m, _ := s.store.Message(res.MessageID); m.State != model.MsgCancelled || !strings.Contains(m.Error, "its session") {
		t.Fatalf("cancelled %+v", m)
	}
	if _, err := r.remind(from(a), fleet.RemindArgs{Cancel: "ci"}); err == nil {
		t.Fatal("cancelled twice")
	}
	if n := noticesTo(s, a); len(n) != 1 {
		t.Fatalf("a notice about the cancel: %+v", n) // only the reminder itself
	}

	// Through the API: listed with read, cancelled by an owner only.
	res, _ = r.remind(from(a), fleet.RemindArgs{InSeconds: 600, Text: "check the build", ID: "build"})
	list := func() []fleet.Reminder {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/sessions/"+a.ID+"/reminders", nil)
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()
		s.handleReminders(w, req)
		var out struct {
			Session   string           `json:"session"`
			Reminders []fleet.Reminder `json:"reminders"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil || out.Session != a.ID {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		return out.Reminders
	}
	if l := list(); len(l) != 1 || l[0].ID != "build" || l[0].MessageID != res.MessageID || !l[0].DueAt.Equal(res.DueAt) || !strings.Contains(l[0].Text, "check the build") {
		t.Fatalf("listed %+v", l)
	}
	cancel := func(ctx context.Context, rid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/sessions/"+a.ID+"/reminders/"+rid+"/cancel", nil).WithContext(ctx)
		req.SetPathValue("id", a.ID)
		req.SetPathValue("rid", rid)
		w := httptest.NewRecorder()
		s.handleCancelReminder(w, req)
		return w
	}
	notOwner := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: "t1"})
	if w := cancel(notOwner, "build"); w.Code != 403 {
		t.Fatalf("non-owner cancel: %d %s", w.Code, w.Body)
	}
	owner := withGrant(context.Background(), grant{scope: scopeAdmin})
	if w := cancel(owner, "nope"); w.Code != 404 {
		t.Fatalf("unknown reminder: %d %s", w.Code, w.Body)
	}
	if w := cancel(owner, res.MessageID); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":true`) {
		t.Fatalf("owner cancel: %d %s", w.Code, w.Body)
	}
	if m, _ := s.store.Message(res.MessageID); m.State != model.MsgCancelled || m.Error != "cancelled by an owner" {
		t.Fatalf("cancelled %+v", m)
	}
	if l := list(); len(l) != 0 {
		t.Fatalf("still listed %+v", l)
	}
}

// Reminders are stored like pending messages: a coordinator restarted
// before one is due still delivers it.
func TestRemindSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *Server {
		st, err := store.OpenFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		s := &Server{store: st, logf: t.Logf, nodes: map[string]*nodeConn{},
			subs: map[string]map[chan model.Event]struct{}{}, fleet: map[chan FleetEvent]struct{}{}, acts: map[string]*activity{}}
		s.router = newRouter(s, Guards{})
		return s
	}
	s := open()
	a := addSession(t, s, "devbox", "a")
	res, err := s.router.remind(from(a), fleet.RemindArgs{InSeconds: 3600, Text: "check CI", ID: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	s.router.close()
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	s = open()
	t.Cleanup(func() {
		s.router.close()
		_ = s.store.Close()
	})
	s.router.failOrphanedAsks() // as at startup: the session is live, so it stays
	if l := s.router.reminders(a.ID); len(l) != 1 || l[0].ID != res.MessageID || l[0].Reminder != "ci" {
		t.Fatalf("after restart %+v", l)
	}
	got := deliveries(t, s, "devbox")
	due(t, s, res.MessageID)
	select {
	case c := <-got:
		if c.ID != res.MessageID || !strings.Contains(c.Text, "check CI") {
			t.Fatalf("delivered %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not delivered after the restart")
	}
	waitState(t, s, res.MessageID, model.MsgDelivered)
}

// A session that stops (or goes) drops its pending reminders: they
// expire, and none is delivered; one that ended while the coordinator
// was down is dropped at startup.
func TestRemindDroppedWhenSessionStops(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "offbox", "a")
	b := addSession(t, s, "offbox", "b")
	r := s.router
	ra, _ := r.remind(from(a), fleet.RemindArgs{InSeconds: 600, Text: "x", ID: "ci"})
	rb, _ := r.remind(from(b), fleet.RemindArgs{InSeconds: 600, Text: "x", ID: "ci"})

	before := a
	stopped, err := s.store.UpdateSession(a.ID, func(x *model.Session) { x.Status = model.StatusStopped })
	if err != nil {
		t.Fatal(err)
	}
	s.projected(before, stopped.ID)
	if m, _ := s.store.Message(ra.MessageID); m.State != model.MsgExpired || m.Error != "its session stopped" {
		t.Fatalf("stopped session's reminder %+v", m)
	}
	if len(r.reminders(a.ID)) != 0 {
		t.Fatal("still pending")
	}
	if msgState(s, rb.MessageID) != model.MsgPending {
		t.Fatal("another session's reminder was dropped")
	}
	if _, err := r.remind(from(stopped), fleet.RemindArgs{Cancel: "ci"}); err == nil {
		t.Fatal("cancelled an expired reminder")
	}

	// b ends with no projection (the coordinator was down): dropped at
	// startup.
	if _, err := s.store.UpdateSession(b.ID, func(x *model.Session) { x.Status = model.StatusGone }); err != nil {
		t.Fatal(err)
	}
	r.failOrphanedAsks()
	if m, _ := s.store.Message(rb.MessageID); m.State != model.MsgExpired || m.Error != "its session gone" {
		t.Fatalf("gone session's reminder %+v", m)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute: "1m", 10 * time.Minute: "10m", 90 * time.Second: "1m30s", time.Hour: "1h",
		90 * time.Minute: "1h30m", 24 * time.Hour: "24h", time.Hour + 30*time.Second: "1h0m30s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
