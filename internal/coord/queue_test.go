// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// #38: a session's queue can be listed, and a queued message promoted or
// cancelled by its sender or an owner, never by another credential.
func TestQueueEndpoints(t *testing.T) {
	s := testServer(t, Guards{})
	var mu sync.Mutex
	var queued []string // what the fake node holds
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		mu.Lock()
		defer mu.Unlock()
		switch cmd.Op {
		case wire.OpDeliver:
			queued = append(queued, cmd.Message.ID)
			return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeQueue})}
		case wire.OpQueue:
			var res wire.QueueResult
			for i, id := range queued {
				res.Messages = append(res.Messages, wire.QueueEntry{ID: id, From: "user:web", Position: i})
			}
			return wire.Result{OK: true, Data: model.Marshal(res)}
		case wire.OpPromote, wire.OpCancel:
			for i, id := range queued {
				if id == cmd.Queued.MessageID {
					queued = append(queued[:i], queued[i+1:]...)
					return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: cmd.Queued.Mode})}
				}
			}
			return wire.Result{Error: "message " + cmd.Queued.MessageID + " is not queued"}
		}
		return wire.Result{Error: "unknown op " + cmd.Op}
	})
	sess := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	tok := store.APIToken{ID: "01M58TOKEN0000000000000000", Name: "laptop", Scope: "operate", Hash: "h1", Created: time.Now()}
	other := store.APIToken{ID: "01M58TOKEN0000000000000001", Name: "phone", Scope: "operate", Hash: "h2", Created: time.Now()}
	for _, x := range []store.APIToken{tok, other} {
		if err := s.store.AddAPIToken(x); err != nil {
			t.Fatal(err)
		}
	}
	asToken := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: tok.ID})
	asOther := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: other.ID})
	asAdmin := withGrant(context.Background(), grant{scope: scopeAdmin})

	sendTo := func(to, text string) model.Message {
		t.Helper()
		from, _ := s.userSender(asToken, "user:web")
		msgs, err := s.router.send(asToken, from, model.MsgNotify, fleet.SendArgs{To: to, Text: text, Mode: model.ModeQueue})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return msgs[0]
	}
	call := func(ctx context.Context, h http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/v1/x/"+id, strings.NewReader(body)).WithContext(ctx)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}
	first, second := sendTo(sess.ID, "first").ID, sendTo(sess.ID, "second").ID

	w := call(asOther, s.handleQueue, "GET", sess.ID, "")
	var q apiclient.Queue
	if err := json.Unmarshal(w.Body.Bytes(), &q); w.Code != 200 || err != nil || len(q.Messages) != 2 ||
		q.Messages[1].ID != second || q.Messages[1].Position != 1 || q.Messages[1].Text != "second" {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}

	if w := call(asOther, s.handleCancel, "POST", first, ""); w.Code != 403 {
		t.Fatalf("another credential cancelled: %d %s", w.Code, w.Body)
	}
	if w := call(asToken, s.handlePromote, "POST", first, `{"mode":"steer"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"mode":"steer"`) {
		t.Fatalf("the sender promoting: %d %s", w.Code, w.Body)
	}
	if m, _ := s.store.Message(first); m.Mode != model.ModeSteer {
		t.Fatalf("promoted message mode %q", m.Mode)
	}
	if w := call(asAdmin, s.handleCancel, "POST", second, ""); w.Code != 200 {
		t.Fatalf("an owner cancelling: %d %s", w.Code, w.Body)
	}
	if m, _ := s.store.Message(second); m.State != model.MsgCancelled || !m.Terminal() {
		t.Fatalf("cancelled message: %+v", m)
	}
	if w := call(asAdmin, s.handleCancel, "POST", second, ""); w.Code != 409 {
		t.Fatalf("cancelling again: %d %s", w.Code, w.Body)
	}

	// A message the coordinator still holds (its host is offline) is
	// cancelled there, and never sent.
	addSession(t, s, "offbox", "b")
	held := sendTo("offbox/b", "later")
	if held.State != model.MsgPending {
		t.Fatalf("held message %s", held.State)
	}
	offb, _ := s.store.Session(held.ToSession)
	w = call(asOther, s.handleQueue, "GET", offb.ID, "")
	q = apiclient.Queue{}
	if err := json.Unmarshal(w.Body.Bytes(), &q); w.Code != 200 || err != nil || !q.Offline || len(q.Messages) != 1 ||
		q.Messages[0].ID != held.ID || !q.Messages[0].Held || q.Messages[0].Text != "later" {
		t.Fatalf("offline queue: %d %s", w.Code, w.Body)
	}
	if w := call(asToken, s.handleCancel, "POST", held.ID, ""); w.Code != 200 {
		t.Fatalf("cancel pending: %d %s", w.Code, w.Body)
	}
	if m, _ := s.store.Message(held.ID); m.State != model.MsgCancelled {
		t.Fatalf("held message %s after cancel", m.State)
	}

	// An older node answers "unknown op": the list says so, and promote
	// gets a 409 that does.
	fakeNode(t, s, "oldbox", func(cmd wire.Cmd) wire.Result { return wire.Result{Error: `unknown op "` + cmd.Op + `"`} })
	old := addSession(t, s, "oldbox", "c", func(x *model.Session) { x.ID = ulid.New() })
	w = call(asOther, s.handleQueue, "GET", old.ID, "")
	q = apiclient.Queue{}
	if err := json.Unmarshal(w.Body.Bytes(), &q); w.Code != 200 || err != nil || !q.NodeTooOld || len(q.Messages) != 0 {
		t.Fatalf("older node: %d %s", w.Code, w.Body)
	}
}

// A node's session.queue reports show in the session's record and in what
// agents see of it.
func TestQueueDepthProjected(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	if _, err := s.ingest("devbox", []model.Event{event(sess.ID, 1, model.KindSessionQueue, model.SessionQueue{Queued: 7})}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.store.Session(sess.ID)
	if got.Queued != 7 || s.info(got).Queued != 7 {
		t.Fatalf("queued %d / %d", got.Queued, s.info(got).Queued)
	}
	if _, err := s.ingest("devbox", []model.Event{event(sess.ID, 2, model.KindSessionQueue, model.SessionQueue{Queued: 0})}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.store.Session(sess.ID); got.Queued != 0 {
		t.Fatalf("queued %d after the queue emptied", got.Queued)
	}
}

// #40: agents may steer (and send in mode auto to) any session, but
// interrupt only their own children; a session that does not accept
// steering gets auto messages, and other agents' steers, queued.
func TestSteerRules(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	other := addSession(t, s, "devbox", "other")
	no := false
	busy := addSession(t, s, "devbox", "busy", func(x *model.Session) { x.AcceptSteer = &no })
	child := addSession(t, s, "devbox", "child", func(x *model.Session) { x.Parent = a.ID; x.AcceptSteer = &no })
	person := sender{user: "user:jm"}

	for _, c := range []struct {
		from sender
		to   model.Session
		mode string
		want string // stored mode, or "" for refused
	}{
		{from(a), other, model.ModeSteer, model.ModeSteer},
		{from(a), other, model.ModeAuto, model.ModeAuto},
		{from(a), other, model.ModeInterrupt, ""},
		{from(a), busy, model.ModeAuto, model.ModeQueue},
		{from(a), busy, model.ModeSteer, model.ModeQueue},
		{from(a), child, model.ModeSteer, model.ModeSteer}, // its own child
		{from(a), child, model.ModeAuto, model.ModeQueue},
		{person, busy, model.ModeAuto, model.ModeQueue},
		{person, busy, model.ModeSteer, model.ModeSteer},
		{person, other, model.ModeInterrupt, model.ModeInterrupt},
	} {
		msgs, err := r.send(ctx, c.from, model.MsgNotify, fleet.SendArgs{To: c.to.ID, Text: "x", Mode: c.mode})
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s %s to %s: accepted", c.from.addr(), c.mode, c.to.Path)
		case c.want != "" && err != nil:
			t.Errorf("%s %s to %s: %v", c.from.addr(), c.mode, c.to.Path, err)
		case c.want != "" && msgs[0].Mode != c.want:
			t.Errorf("%s %s to %s: mode %q, want %q", c.from.addr(), c.mode, c.to.Path, msgs[0].Mode, c.want)
		}
	}

	// fleet_send without a mode is auto.
	res, err := s.fleetOp(ctx, from(a), "fleet.send", model.Marshal(fleet.SendArgs{To: other.ID, Text: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := s.store.Message(res.(fleet.SendResult).Messages[0].ID); m.Mode != model.ModeAuto {
		t.Fatalf("fleet_send default mode %q", m.Mode)
	}
}
