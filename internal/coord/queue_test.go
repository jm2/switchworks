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

// A queued message belongs to the token that sent it, by id: a token made
// later under a revoked one's name does not inherit its messages. A record
// from before ids were stored matches by name only a token that already
// existed when it was sent.
func TestQueuedMessageTokenReuse(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "offbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	old := store.APIToken{ID: ulid.New(), Name: "laptop", Scope: "operate", Hash: "h1", Created: time.Now().Add(-time.Hour)}
	if err := s.store.AddAPIToken(old); err != nil {
		t.Fatal(err)
	}
	asOld := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: old.ID})
	from, _ := s.userSender(asOld, "user:web")
	msgs, err := s.router.send(asOld, from, model.MsgNotify, fleet.SendArgs{To: sess.ID, Text: "x"})
	if err != nil || msgs[0].ViaToken != old.ID || msgs[0].Via != "token:laptop" {
		t.Fatalf("send: %+v %v", msgs, err)
	}
	legacy := msgs[0]
	legacy.ID, legacy.ViaToken = ulid.New(), ""
	if err := s.store.PutMessage(legacy); err != nil {
		t.Fatal(err)
	}

	if err := s.store.DeleteAPIToken(old.ID); err != nil {
		t.Fatal(err)
	}
	reused := store.APIToken{ID: ulid.New(), Name: "laptop", Scope: "operate", Hash: "h2", Created: time.Now().Add(time.Second)}
	if err := s.store.AddAPIToken(reused); err != nil {
		t.Fatal(err)
	}
	asReused := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: reused.ID})
	cancel := func(ctx context.Context, id string) int {
		r := httptest.NewRequest("POST", "/v1/messages/"+id+"/cancel", nil).WithContext(ctx)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.handleCancel(w, r)
		return w.Code
	}
	for _, id := range []string{msgs[0].ID, legacy.ID} {
		if code := cancel(asReused, id); code != 403 {
			t.Fatalf("a token reusing the name cancelled %s: %d", id, code)
		}
	}
	// Its own messages are its to cancel.
	from, _ = s.userSender(asReused, "user:web")
	mine, err := s.router.send(asReused, from, model.MsgNotify, fleet.SendArgs{To: sess.ID, Text: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if code := cancel(asReused, mine[0].ID); code != 200 {
		t.Fatalf("cancelling its own: %d", code)
	}
	legacy.ID = ulid.New()
	legacy.CreatedAt = time.Now().Add(2 * time.Second)
	if err := s.store.PutMessage(legacy); err != nil {
		t.Fatal(err)
	}
	if code := cancel(asReused, legacy.ID); code != 200 {
		t.Fatalf("an old-style record sent after the token was made: %d", code)
	}
}

// #63: asks, replies, completion notices and Switchworks's notices go in
// mode auto like sends, so they reach a running turn; a session that does
// not accept steering still gets them queued.
func TestEveryKindSteers(t *testing.T) {
	ctx := context.Background()
	for _, accept := range []bool{true, false} {
		s := testServer(t, Guards{})
		r := s.router
		lead := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.AcceptSteer = &accept })
		peer := addSession(t, s, "devbox", "peer", func(x *model.Session) { x.AcceptSteer = &accept })
		child := addSession(t, s, "devbox", "child", func(x *model.Session) { x.Parent = lead.ID })
		want := model.ModeAuto
		if !accept {
			want = model.ModeQueue
		}
		mode := func(sess model.Session, kind string) string {
			t.Helper()
			for _, m := range s.store.Messages(store.MessageFilter{Session: sess.ID}) {
				if m.Kind == kind && m.ToSession == sess.ID {
					return m.Mode
				}
			}
			t.Fatalf("no %s to %s", kind, sess.Path)
			return ""
		}

		zero := 0
		res := r.ask(ctx, from(lead), fleet.AskArgs{To: peer.ID, Text: "status?", WaitSeconds: &zero})
		if res.Status == fleet.AskFailed {
			t.Fatal(res.Error)
		}
		if _, err := r.reply(ctx, from(peer), fleet.ReplyArgs{MessageID: res.MessageID, Text: "fine"}); err != nil {
			t.Fatal(err)
		}
		r.onSettled(child, model.Settled{TurnStatus: "completed"})
		r.notifySender(model.Message{ID: ulid.New(), Kind: model.MsgNotify, FromSession: peer.ID, To: "x"}, "your message expired")

		for _, c := range []struct {
			to   model.Session
			kind string
		}{
			{peer, model.MsgAsk},
			{lead, model.MsgReply},
			{lead, model.MsgDone},
			{peer, model.MsgSystem},
		} {
			if got := mode(c.to, c.kind); got != want {
				t.Errorf("accept_steer %v: %s to %s has mode %q, want %q", accept, c.kind, c.to.Path, got, want)
			}
		}
	}
}

// #47: a message with a supersedes key replaces the sender's earlier one
// with that key to the same session while the coordinator still holds
// it; another key, sender, credential or session does not.
func TestSupersedePending(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "offbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	c := addSession(t, s, "offbox", "c", func(x *model.Session) { x.ID = ulid.New() })
	addSession(t, s, "offbox", "b")
	addSession(t, s, "offbox", "d")
	ctx := context.Background()
	send := func(from sender, to, key string) model.Message {
		t.Helper()
		msgs, err := s.router.send(ctx, from, model.MsgNotify, fleet.SendArgs{To: to, Text: "status", Supersedes: key})
		if err != nil || msgs[0].State != model.MsgPending {
			t.Fatalf("send: %+v %v", msgs, err)
		}
		return msgs[0]
	}
	first := send(from(a), "b", "status:gate5")
	otherKey := send(from(a), "b", "status:gate6")
	otherSender := send(from(c), "b", "status:gate5")
	otherSession := send(from(a), "offbox/d", "status:gate5")
	unkeyed := send(from(a), "b", "")
	second := send(from(a), "b", "status:gate5")
	if m, _ := s.store.Message(first.ID); m.State != model.MsgSuperseded || m.SupersededBy != second.ID || !m.Terminal() {
		t.Fatalf("superseded message: %+v", m)
	}
	for _, m := range []model.Message{otherKey, otherSender, otherSession, unkeyed, second} {
		if st := msgState(s, m.ID); st != model.MsgPending {
			t.Fatalf("%s (%s to %s) is %s", m.ID, m.Supersedes, m.ToPath, st)
		}
	}
	if n := noticesTo(s, a); len(n) != 0 {
		t.Fatalf("the sender was told: %+v", n)
	}
	// A superseded message is never delivered when its host returns.
	s.router.deliver(ctx, first)
	if st := msgState(s, first.ID); st != model.MsgSuperseded {
		t.Fatalf("after a delivery attempt: %s", st)
	}

	// People: the same name with another credential is another sender.
	web := sender{user: "user:web", via: "token:laptop", token: "01M58TOKEN0000000000000000"}
	phone := sender{user: "user:web", via: "token:phone", token: "01M58TOKEN0000000000000001"}
	p1 := send(web, "b", "deploy")
	send(phone, "b", "deploy")
	if st := msgState(s, p1.ID); st != model.MsgPending {
		t.Fatalf("another credential superseded a person's message: %s", st)
	}
	p2 := send(web, "b", "deploy")
	if m, _ := s.store.Message(p1.ID); m.State != model.MsgSuperseded || m.SupersededBy != p2.ID {
		t.Fatalf("person's message: %+v", m)
	}

	if _, err := s.router.send(ctx, from(a), model.MsgAsk, fleet.SendArgs{To: "b", Text: "?", Supersedes: "q"}); err == nil {
		t.Fatal("an ask took a supersedes key")
	}
	if _, err := s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x", Supersedes: strings.Repeat("k", fleet.MaxSupersedesBytes+1)}); err == nil {
		t.Fatal("an overlong key was taken")
	}
}

// #47: a superseded message waiting in the node's queue is cancelled there
// (naming the newer one) and becomes superseded; one that went into a
// turn, or that the node no longer holds, is left.
func TestSupersedeQueued(t *testing.T) {
	s := testServer(t, Guards{})
	var mu sync.Mutex
	var queued []string            // what the fake node holds
	cancels := map[string]string{} // cancelled id -> superseded_by
	steer := false                 // the next delivery goes into a turn
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		mu.Lock()
		defer mu.Unlock()
		switch cmd.Op {
		case wire.OpDeliver:
			if steer {
				return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeSteer})}
			}
			queued = append(queued, cmd.Message.ID)
			return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeQueue})}
		case wire.OpCancel:
			for i, id := range queued {
				if id == cmd.Queued.MessageID {
					queued = append(queued[:i], queued[i+1:]...)
					cancels[id] = cmd.Queued.SupersededBy
					return wire.Result{OK: true}
				}
			}
			return wire.Result{Error: "message " + cmd.Queued.MessageID + " is not queued"}
		}
		return wire.Result{Error: "unknown op " + cmd.Op}
	})
	a := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	addSession(t, s, "devbox", "b", func(x *model.Session) { x.ID = ulid.New() })
	ctx := context.Background()
	send := func(text string) model.Message {
		t.Helper()
		msgs, err := s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/b", Text: text, Mode: model.ModeQueue, Supersedes: "status:gate5"})
		if err != nil || msgs[0].State != model.MsgDelivered {
			t.Fatalf("send: %+v %v", msgs, err)
		}
		return msgs[0]
	}
	first := send("40%")
	second := send("80%")
	if m, _ := s.store.Message(first.ID); m.State != model.MsgSuperseded || m.SupersededBy != second.ID {
		t.Fatalf("queued message: %+v", m)
	}
	mu.Lock()
	if len(queued) != 1 || queued[0] != second.ID || cancels[first.ID] != second.ID {
		t.Fatalf("node queue %v, cancels %v", queued, cancels)
	}
	// The node took the second into a turn (it no longer holds it), and
	// the next is steered in: neither is superseded.
	queued, steer = nil, true
	mu.Unlock()
	third := send("100%")
	if st := msgState(s, second.ID); st != model.MsgDelivered {
		t.Fatalf("a message the node no longer holds is %s", st)
	}
	send("done")
	if st := msgState(s, third.ID); st != model.MsgDelivered {
		t.Fatalf("a message steered into a turn is %s", st)
	}
}

// A reply to an agent's ask carries the start of the ask, both to the
// node that delivers it and in fleet_inbox.
func TestReplyQuotesAsk(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	lead := addSession(t, s, "devbox", "lead")
	zero := 0
	res := r.ask(ctx, from(lead), fleet.AskArgs{To: "user:jm", Text: "May I close #281 and #282?", WaitSeconds: &zero})
	if res.Status == fleet.AskFailed {
		t.Fatal(res.Error)
	}
	if got := r.askText(res.MessageID); got != "May I close #281 and #282?" {
		t.Fatalf("askText: %q", got)
	}
	if _, err := r.reply(ctx, sender{user: "user:jm"}, fleet.ReplyArgs{MessageID: res.MessageID, Text: "1. yes"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.store.Messages(store.MessageFilter{Session: lead.ID}) {
		if m.Kind == model.MsgReply && r.askText(m.ReplyTo) == "" {
			t.Fatalf("reply %s does not lead back to its ask's text", m.ID)
		}
	}
	if r.askText("nope") != "" || r.askText("") != "" {
		t.Fatal("askText of no ask")
	}
}

// A quiet notify reaches the node marked quiet; asks and interrupts
// cannot be quiet.
func TestQuietSend(t *testing.T) {
	s := testServer(t, Guards{})
	var mu sync.Mutex
	var got []wire.MessageCmd
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		mu.Lock()
		defer mu.Unlock()
		if cmd.Op == wire.OpDeliver {
			got = append(got, *cmd.Message)
			return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeQueue})}
		}
		return wire.Result{Error: "unknown op " + cmd.Op}
	})
	a := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	addSession(t, s, "devbox", "b", func(x *model.Session) { x.ID = ulid.New() })
	ctx := context.Background()
	msgs, err := s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/b", Text: "ok", Mode: model.ModeAuto, Quiet: true})
	if err != nil || len(msgs) != 1 || !msgs[0].Quiet || msgs[0].State != model.MsgDelivered {
		t.Fatalf("send: %+v %v", msgs, err)
	}
	if m, _ := s.store.Message(msgs[0].ID); !m.Quiet {
		t.Fatal("the stored message is not quiet")
	}
	mu.Lock()
	if len(got) != 1 || !got[0].Quiet {
		t.Fatalf("node got %+v", got)
	}
	mu.Unlock()
	if _, err := s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/b", Text: "x", Mode: model.ModeInterrupt, Quiet: true}); err == nil {
		t.Fatal("a quiet interrupt was sent")
	}
	if _, err := s.router.send(ctx, from(a), model.MsgAsk, fleet.SendArgs{To: "devbox/b", Text: "x?", Mode: model.ModeAuto, Quiet: true}); err == nil {
		t.Fatal("a quiet ask was sent")
	}
}
