// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// #46: message.expired (the node dropped a queued message past its expiry)
// expires it and gives an agent sender the expiry notice; a node cannot
// expire another session's messages.
func TestMessageExpiredEvent(t *testing.T) {
	s := testServer(t, Guards{})
	lead := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.ID = ulid.New() })
	worker := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	bystander := addSession(t, s, "devbox", "other", func(x *model.Session) { x.ID = ulid.New() })
	ask := putAsk(t, s, lead, worker)
	if _, err := s.ingest("devbox", []model.Event{event(bystander.ID, 1, model.KindMessageExpired,
		model.MessageEvent{MessageID: ask.ID})}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, ask.ID) != model.MsgDelivered {
		t.Fatal("another session's message.expired expired the ask")
	}
	if _, err := s.ingest("devbox", []model.Event{event(worker.ID, 1, model.KindMessageExpired,
		model.MessageEvent{MessageID: ask.ID})}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, ask.ID) != model.MsgExpired {
		t.Fatalf("ask %s", msgState(s, ask.ID))
	}
	if n := noticesTo(s, lead); len(n) != 1 || !strings.Contains(n[0].Text, "expired before it could be delivered") {
		t.Fatalf("notices %+v", n)
	}
	res, err := s.router.inbox(context.Background(), from(lead), fleet.InboxArgs{Ticket: ask.ID})
	if err != nil || res.Ticket.Status != fleet.AskFailed {
		t.Fatalf("ticket %+v %v", res.Ticket, err)
	}
	// Reported again (a resent event): no second notice.
	if _, err := s.ingest("devbox", []model.Event{event(worker.ID, 2, model.KindMessageExpired,
		model.MessageEvent{MessageID: ask.ID})}); err != nil {
		t.Fatal(err)
	}
	if n := noticesTo(s, lead); len(n) != 1 {
		t.Fatalf("notices %+v", n)
	}
}

// #46: the node is given each message's expiry, and a steer it reports
// expired (dropped while it waited) is expired, not delivered.
func TestDeliverCarriesExpiry(t *testing.T) {
	s := testServer(t, Guards{})
	got := make(chan wire.MessageCmd, 4)
	var expired atomic.Bool
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		if cmd.Message != nil {
			got <- *cmd.Message
		}
		return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeSteer, Expired: expired.Load()})}
	})
	a := addSession(t, s, "devbox", "a")
	addSession(t, s, "devbox", "b")
	ctx := context.Background()
	msgs, err := s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x", TTLSeconds: 60})
	if err != nil || msgs[0].State != model.MsgDelivered {
		t.Fatalf("send: %+v %v", msgs, err)
	}
	if c := <-got; c.ExpiresAt.IsZero() || !c.ExpiresAt.Equal(msgs[0].ExpiresAt) {
		t.Fatalf("expires_at %v, want %v", c.ExpiresAt, msgs[0].ExpiresAt)
	}
	expired.Store(true)
	msgs, err = s.router.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "y"})
	if err != nil || msgs[0].State != model.MsgExpired {
		t.Fatalf("send: %+v %v", msgs, err)
	}
	if n := noticesTo(s, a); len(n) != 1 || !strings.Contains(n[0].Text, "queue") {
		t.Fatalf("notices %+v", n)
	}
}
