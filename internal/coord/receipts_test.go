// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// #45: a steered message is delivered at once (delivered_at in the send's
// result); a queued one gets delivered_at when the node reports it went
// into a turn, and is seen when that turn completes. The sender's inbox
// shows both. Another session cannot mark it seen.
func TestReceipts(t *testing.T) {
	s := testServer(t, Guards{})
	var mode atomic.Value
	mode.Store(model.ModeQueue)
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result {
		return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: mode.Load().(string)})}
	})
	a := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	b := addSession(t, s, "devbox", "b", func(x *model.Session) { x.ID = ulid.New() })
	ctx := context.Background()
	send := func(text string) fleet.MessageInfo {
		t.Helper()
		res, err := s.router.sendEach(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: text})
		if err != nil || len(res.Messages) != 1 {
			t.Fatalf("send: %+v %v", res, err)
		}
		return res.Messages[0]
	}

	q := send("queued")
	if q.State != model.MsgDelivered || !q.DeliveredAt.IsZero() {
		t.Fatalf("queued: %+v", q)
	}
	if _, err := s.ingest("devbox", []model.Event{event(b.ID, 1, model.KindMessageDelivered, model.MessageEvent{MessageID: q.ID})}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.store.Message(q.ID); m.DeliveredAt.IsZero() || m.State != model.MsgDelivered {
		t.Fatalf("entered its turn: %+v", m)
	}
	if _, err := s.ingest("devbox", []model.Event{event(a.ID, 1, model.KindMessageSeen,
		model.MessageSeen{MessageID: q.ID, TurnID: "t0"})}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, q.ID) != model.MsgDelivered {
		t.Fatal("another session's message.seen marked it seen")
	}
	if _, err := s.ingest("devbox", []model.Event{event(b.ID, 2, model.KindMessageSeen,
		model.MessageSeen{MessageID: q.ID, TurnID: "t1", ItemID: "i1"})}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.store.Message(q.ID)
	if m.State != model.MsgSeen || m.SeenAt.IsZero() || m.SeenTurn != "t1" || m.SeenItem != "i1" || m.SeenAt.Before(m.DeliveredAt) {
		t.Fatalf("seen: %+v", m)
	}

	mode.Store(model.ModeSteer)
	st := send("steered")
	if st.State != model.MsgDelivered || st.DeliveredAt.IsZero() {
		t.Fatalf("steered: %+v", st)
	}

	res, err := s.router.inbox(ctx, from(a), fleet.InboxArgs{All: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fleet.InboxMessage{}
	for _, x := range compact(t, res) {
		got[x.ID] = x
	}
	if x := got[q.ID]; x.State != model.MsgSeen || x.DeliveredAt.IsZero() || x.SeenAt.IsZero() {
		t.Fatalf("inbox, queued: %+v", x)
	}
	if x := got[st.ID]; x.State != model.MsgDelivered || x.DeliveredAt.IsZero() || !x.SeenAt.IsZero() {
		t.Fatalf("inbox, steered: %+v", x)
	}
	// The recipient's view of messages to it carries no receipts.
	res, err = s.router.inbox(ctx, from(b), fleet.InboxArgs{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range compact(t, res) {
		if x.State != "" {
			t.Fatalf("recipient's inbox: %+v", x)
		}
	}
}

// #45: an ask can be seen and still answered, and stays answered when its
// turn completes after the reply; a seen ask whose target stops fails.
func TestSeenAsks(t *testing.T) {
	s := testServer(t, Guards{})
	lead := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.ID = ulid.New() })
	worker := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	ctx := context.Background()
	seen := func(id string, seq int64) {
		t.Helper()
		if _, err := s.ingest("devbox", []model.Event{event(worker.ID, seq, model.KindMessageSeen,
			model.MessageSeen{MessageID: id, TurnID: "t", ItemID: "i"})}); err != nil {
			t.Fatal(err)
		}
	}

	first := putAsk(t, s, lead, worker)
	seen(first.ID, 1)
	if msgState(s, first.ID) != model.MsgSeen {
		t.Fatalf("ask %s", msgState(s, first.ID))
	}
	if _, err := s.router.reply(ctx, from(worker), fleet.ReplyArgs{MessageID: first.ID, Text: "42"}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, first.ID) != model.MsgAnswered {
		t.Fatalf("ask %s", msgState(s, first.ID))
	}

	second := putAsk(t, s, lead, worker)
	if _, err := s.router.reply(ctx, from(worker), fleet.ReplyArgs{MessageID: second.ID, Text: "43"}); err != nil {
		t.Fatal(err)
	}
	seen(second.ID, 2)
	if m, _ := s.store.Message(second.ID); m.State != model.MsgAnswered || m.SeenAt.IsZero() {
		t.Fatalf("answered, then seen: %+v", m)
	}

	third := putAsk(t, s, lead, worker)
	seen(third.ID, 3)
	if _, err := s.ingest("devbox", []model.Event{status(worker.ID, 4, model.StatusStopped)}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, third.ID) != model.MsgFailed {
		t.Fatalf("seen ask to a stopped session: %s", msgState(s, third.ID))
	}
}
