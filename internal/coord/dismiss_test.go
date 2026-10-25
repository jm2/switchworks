// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
)

// inboxCall runs an Inbox handler for message id as the credential in ctx.
func inboxCall(t *testing.T, ctx context.Context, h http.HandlerFunc, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/messages/"+id, strings.NewReader(body)).WithContext(ctx)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// #61: dismissing an ask a fleet_ask is waiting on returns it at once,
// "dismissed without an answer"; the asker was told, so there is no notice
// and no undo, and a later reply gets 409.
func TestDismissWakesWaitingAsk(t *testing.T) {
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "offbox", "a")
	asAdmin := withGrant(context.Background(), grant{scope: scopeAdmin})

	wait := 60
	got := make(chan fleet.AskResult, 1)
	go func() {
		got <- r.ask(context.Background(), from(a), fleet.AskArgs{To: "user:jm", Text: "ship it?", WaitSeconds: &wait})
	}()
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		for k := range r.waiters {
			id = k
		}
		r.mu.Unlock()
	}
	if id == "" {
		t.Fatal("the ask never waited")
	}

	start := time.Now()
	w := inboxCall(t, asAdmin, s.handleDismiss, id, `{"from":"user:web"}`)
	if w.Code != 200 {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body)
	}
	select {
	case res := <-got:
		if res.Status != fleet.AskFailed || !strings.Contains(res.Error, "dismissed without an answer by user:web (admin)") {
			t.Fatalf("ask result: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting ask was not woken")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("woken after %v", time.Since(start))
	}
	m, _ := s.store.Message(id)
	if m.State != model.MsgDismissed || !m.Terminal() || !m.Inline || m.DismissedBy != "user:web (admin)" || m.DismissedAt.IsZero() || m.NoticeID != "" {
		t.Fatalf("dismissed ask: %+v", m)
	}
	if w := inboxCall(t, asAdmin, s.handleUndismiss, id, ""); w.Code != 409 {
		t.Fatalf("undo after the asker was told: %d %s", w.Code, w.Body)
	}
	if w := inboxCall(t, asAdmin, s.handleReplyMessage, id, `{"text":"yes after all"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "dismissed") {
		t.Fatalf("late reply: %d %s", w.Code, w.Body)
	}
	if w := inboxCall(t, asAdmin, s.handleDismiss, id, ""); w.Code != 409 {
		t.Fatalf("dismissing again: %d %s", w.Code, w.Body)
	}
	// Only asks to people can be dismissed.
	other := storeMsg(t, s, a, 1, func(x *model.Message) { x.Kind = model.MsgAsk })
	if w := inboxCall(t, asAdmin, s.handleDismiss, other.ID, ""); w.Code != 404 {
		t.Fatalf("dismissing an ask to a session: %d %s", w.Code, w.Body)
	}
}

// An asker whose wait has ended gets a notice, held back for the undo
// window; an undo withdraws it unsent, and once it has gone (or the asker
// saw the dismissal with its ticket) the dismissal stands.
func TestDismissNoticeAndUndo(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "offbox", "a")
	asAdmin := withGrant(ctx, grant{scope: scopeAdmin})
	ask := func() string {
		t.Helper()
		res := r.ask(ctx, from(a), fleet.AskArgs{To: "user:jm", Text: "q?", WaitSeconds: new(int)})
		if res.Status != fleet.AskPending {
			t.Fatalf("ask: %+v", res)
		}
		return res.MessageID
	}
	dismiss := func(id string) model.Message {
		t.Helper()
		w := inboxCall(t, asAdmin, s.handleDismiss, id, "")
		var m model.Message
		if err := json.Unmarshal(w.Body.Bytes(), &m); w.Code != 200 || err != nil {
			t.Fatalf("dismiss: %d %s", w.Code, w.Body)
		}
		return m
	}

	id := ask()
	m := dismiss(id)
	n, ok := s.store.Message(m.NoticeID)
	if m.State != model.MsgDismissed || m.Inline || m.DismissedBy != "user:cli (admin)" || !ok {
		t.Fatalf("dismissed ask: %+v", m)
	}
	if n.Kind != model.MsgSystem || n.ToSession != a.ID || n.State != model.MsgPending || !strings.Contains(n.Text, "dismissed by user:cli (admin) without an answer") ||
		n.NotBefore.Sub(m.DismissedAt) < dismissUndo-time.Second {
		t.Fatalf("notice: %+v", n)
	}

	// Undone before the notice went: it is withdrawn and the ask is open
	// again, to be answered.
	if w := inboxCall(t, asAdmin, s.handleUndismiss, id, ""); w.Code != 200 {
		t.Fatalf("undo: %d %s", w.Code, w.Body)
	}
	if m, _ := s.store.Message(id); m.State != model.MsgDelivered || m.Error != "" || m.DismissedBy != "" || m.NoticeID != "" {
		t.Fatalf("ask after undo: %+v", m)
	}
	if n, _ := s.store.Message(n.ID); n.State != model.MsgCancelled {
		t.Fatalf("notice after undo: %+v", n)
	}
	if w := inboxCall(t, asAdmin, s.handleUndismiss, id, ""); w.Code != 409 {
		t.Fatalf("undo of an open ask: %d %s", w.Code, w.Body)
	}
	if _, err := r.reply(ctx, sender{user: "user:web"}, fleet.ReplyArgs{MessageID: id, Text: "yes"}); err != nil {
		t.Fatalf("reply after undo: %v", err)
	}

	// The notice has gone: no undo.
	id = ask()
	m = dismiss(id)
	n, _ = s.store.Message(m.NoticeID)
	n.State = model.MsgDelivered
	if err := s.store.PutMessage(n); err != nil {
		t.Fatal(err)
	}
	if w := inboxCall(t, asAdmin, s.handleUndismiss, id, ""); w.Code != 409 {
		t.Fatalf("undo after the notice went: %d %s", w.Code, w.Body)
	}

	// The asker checks its ticket first: it learns of it there, so the
	// notice is not needed and the dismissal cannot be undone.
	id = ask()
	m = dismiss(id)
	res, err := r.inbox(ctx, from(a), fleet.InboxArgs{Ticket: id})
	if err != nil || res.Ticket == nil || res.Ticket.Status != fleet.AskFailed || !strings.Contains(res.Ticket.Error, "dismissed without an answer") {
		t.Fatalf("ticket: %+v %v", res.Ticket, err)
	}
	if n, _ := s.store.Message(m.NoticeID); n.State != model.MsgCancelled {
		t.Fatalf("notice after the ticket: %+v", n)
	}
	if w := inboxCall(t, asAdmin, s.handleUndismiss, id, ""); w.Code != 409 {
		t.Fatalf("undo after the ticket: %d %s", w.Code, w.Body)
	}
	if _, err := r.reply(ctx, sender{user: "user:web"}, fleet.ReplyArgs{MessageID: id, Text: "late"}); err == nil {
		t.Fatal("a reply to a dismissed ask went through")
	}
}

// Read marks are kept by the coordinator, on the message: one client marks
// a message read and another sees it so; a read ask is still open.
func TestInboxReadAcrossClients(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "offbox", "a")
	laptop := store.APIToken{ID: "01M61TOKEN0000000000000000", Name: "laptop", Scope: "operate", Hash: "h1", Created: time.Now()}
	phone := store.APIToken{ID: "01M61TOKEN0000000000000001", Name: "phone", Scope: "read", Hash: "h2", Created: time.Now()}
	for _, x := range []store.APIToken{laptop, phone} {
		if err := s.store.AddAPIToken(x); err != nil {
			t.Fatal(err)
		}
	}
	asLaptop := withGrant(ctx, grant{scope: scopeOperate, tokenID: laptop.ID})
	asPhone := withGrant(ctx, grant{scope: scopeRead, tokenID: phone.ID})

	note, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "user:jm", Text: "done"})
	if err != nil {
		t.Fatal(err)
	}
	asked := r.ask(ctx, from(a), fleet.AskArgs{To: "user:jm", Text: "q?", WaitSeconds: new(int)})
	for _, id := range []string{note[0].ID, asked.MessageID} {
		if w := inboxCall(t, asLaptop, s.handleMarkRead, id, `{"from":"user:web"}`); w.Code != 200 {
			t.Fatalf("mark read: %d %s", w.Code, w.Body)
		}
	}
	first, _ := s.store.Message(asked.MessageID)
	if w := inboxCall(t, asLaptop, s.handleMarkRead, asked.MessageID, ""); w.Code != 200 {
		t.Fatalf("mark read again: %d %s", w.Code, w.Body)
	}

	req := httptest.NewRequest("GET", "/v1/messages?inbox=1", nil).WithContext(asPhone)
	w := httptest.NewRecorder()
	s.handleListMessages(w, req)
	var msgs []model.Message
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil || len(msgs) != 2 {
		t.Fatalf("inbox from the other client: %d %s", w.Code, w.Body)
	}
	for _, m := range msgs {
		if m.ReadAt.IsZero() || m.ReadBy != "user:web (token:laptop)" {
			t.Fatalf("read mark seen by the other client: %+v", m)
		}
		if m.ID == asked.MessageID && (!m.ReadAt.Equal(first.ReadAt) || m.State != model.MsgDelivered || !m.Accepted()) {
			t.Fatalf("read ask: %+v", m)
		}
	}
	// Still open: it can be answered.
	if _, err := r.reply(ctx, sender{user: "user:web"}, fleet.ReplyArgs{MessageID: asked.MessageID, Text: "yes"}); err != nil {
		t.Fatalf("reply to a read ask: %v", err)
	}
	// Only messages to people have read marks.
	other := storeMsg(t, s, a, 1)
	if w := inboxCall(t, asLaptop, s.handleMarkRead, other.ID, ""); w.Code != 404 {
		t.Fatalf("mark read a message to a session: %d %s", w.Code, w.Body)
	}
}
