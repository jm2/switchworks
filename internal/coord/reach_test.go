// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// fleet_inbox shows a session only the messages that went into its input:
// not one waiting in its queue (it arrives as a turn, and would be handled
// twice), and not one cancelled, expired or failed before it got there.
func TestInboxOnlyWhatReachedIt(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	addSession(t, s, "offbox", "a")
	b := addSession(t, s, "offbox", "b")
	queued := storeMsg(t, s, b, 1, func(m *model.Message) { m.Text, m.DeliveredAt = "queued", time.Time{} })
	for _, st := range []string{model.MsgCancelled, model.MsgExpired, model.MsgFailed} {
		storeMsg(t, s, b, 1, func(m *model.Message) { m.Text, m.State, m.DeliveredAt = st, st, time.Time{} })
	}
	// A steer that failed after its node took it still never reached it.
	storeMsg(t, s, b, 1, func(m *model.Message) { m.Text, m.State = "failed steer", model.MsgFailed })
	storeMsg(t, s, b, 1, func(m *model.Message) { m.Text = "steered" })
	if _, err := r.send(ctx, from(b), model.MsgNotify, fleet.SendArgs{To: "a", Text: "sent"}); err != nil {
		t.Fatal(err)
	}

	all, err := r.inbox(ctx, from(b), fleet.InboxArgs{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(compact(t, all)); got != "sent,steered" {
		t.Fatalf("all: %q", got)
	}
	res, err := r.inbox(ctx, from(b), fleet.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(compact(t, res)); got != "steered" {
		t.Fatalf("unread: %q", got)
	}
	// The queued one enters its turn: it is read there, not listed after.
	r.onDelivered(b.ID, queued.ID, "queue")
	r.entered(b.ID, queued.ID, time.Now())
	if res, _ := r.inbox(ctx, from(b), fleet.InboxArgs{}); len(compact(t, res)) != 0 {
		t.Fatalf("listed after its turn: %+v", res.Messages)
	}
}

// An ask whose sender was told it will not be answered (cancelled, expired
// or failed) cannot be answered after all.
func TestReplyToEndedAsk(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "offbox", "a")
	b := addSession(t, s, "offbox", "b")
	for _, st := range []string{model.MsgCancelled, model.MsgExpired, model.MsgFailed} {
		ask := storeMsg(t, s, b, 1, func(m *model.Message) {
			m.Kind, m.From, m.FromSession, m.State, m.Error = model.MsgAsk, a.Path, a.ID, st, "ended"
		})
		_, err := r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: ask.ID, Text: "late"})
		if err == nil || !strings.Contains(err.Error(), "can no longer be answered") {
			t.Fatalf("reply to a %s ask: %v", st, err)
		}
		if cur, _ := s.store.Message(ask.ID); cur.State != st || cur.ReplyID != "" {
			t.Fatalf("%s ask after a refused reply: %+v", st, cur)
		}
	}
}

// A message the node reports delivered again (a retry after a busy
// harness), or only after its turn was seen, keeps its first receipt.
func TestLateAndRepeatedDelivered(t *testing.T) {
	s := testServer(t, Guards{})
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result {
		return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeQueue})}
	})
	a := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	b := addSession(t, s, "devbox", "b", func(x *model.Session) { x.ID = ulid.New() })
	msgs, err := s.router.send(context.Background(), from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id := msgs[0].ID
	ingest := func(seq int64, kind string, p any) {
		t.Helper()
		if _, err := s.ingest("devbox", []model.Event{event(b.ID, seq, kind, p)}); err != nil {
			t.Fatal(err)
		}
	}
	ingest(1, model.KindMessageDelivered, model.MessageEvent{MessageID: id, Mode: "queue"})
	first, _ := s.store.Message(id)
	time.Sleep(10 * time.Millisecond)
	ingest(2, model.KindMessageDelivered, model.MessageEvent{MessageID: id, Mode: "queue"})
	if m, _ := s.store.Message(id); !m.DeliveredAt.Equal(first.DeliveredAt) || m.State != model.MsgDelivered {
		t.Fatalf("delivered again: %+v", m)
	}
	ingest(3, model.KindMessageSeen, model.MessageSeen{MessageID: id, TurnID: "t1"})
	ingest(4, model.KindSessionStatus, model.SessionStatus{Status: model.StatusIdle})
	ingest(5, model.KindMessageDelivered, model.MessageEvent{MessageID: id, Mode: "queue"})
	m, _ := s.store.Message(id)
	if m.State != model.MsgSeen || !m.DeliveredAt.Equal(first.DeliveredAt) || m.SeenTurn != "t1" {
		t.Fatalf("delivered after seen: %+v", m)
	}
}
