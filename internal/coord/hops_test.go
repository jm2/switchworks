// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

// storeMsg stores a message to sess with the given hops, as delivered
// (into its input) unless mut says otherwise.
func storeMsg(t *testing.T, s *Server, sess model.Session, hops int, mut ...func(*model.Message)) model.Message {
	t.Helper()
	now := time.Now().UTC()
	m := model.Message{ID: ulid.New(), Kind: model.MsgNotify, From: "devbox/x", To: sess.Path, ToSession: sess.ID, ToPath: sess.Path,
		Text: "x", Hops: hops, State: model.MsgDelivered, CreatedAt: now, ExpiresAt: now.Add(time.Hour), DeliveredAt: now}
	for _, f := range mut {
		f(&m)
	}
	if err := s.store.PutMessage(m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A turn counts the deepest hops of everything it took in: a batch, or a
// steer, carrying a shallow message cannot reset a long chain. Only a turn
// that starts after the last one ended starts the count over.
func TestHopsKeepTheDeepest(t *testing.T) {
	s := testServer(t, Guards{MaxHops: 8})
	r := s.router
	b := addSession(t, s, "devbox", "b")
	hops := func() int { t.Helper(); return r.hopsFor(from(b)) - 1 }

	deep, shallow := storeMsg(t, s, b, 8), storeMsg(t, s, b, 1)
	r.onDelivered(b.ID, deep.ID, "queue") // one batch
	r.onDelivered(b.ID, shallow.ID, "queue")
	if got := hops(); got != 8 {
		t.Fatalf("batch [8, 1]: hops %d", got)
	}
	r.onDelivered(b.ID, storeMsg(t, s, b, 0).ID, model.ModeSteer)
	if got := hops(); got != 8 {
		t.Fatalf("after a shallow steer: hops %d", got)
	}
	if _, err := r.send(context.Background(), from(b), model.MsgNotify, fleet.SendArgs{To: "user:jm", Text: "x"}); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("send at the limit: %v", err)
	}
	// The same message reported again changes nothing.
	r.onDelivered(b.ID, shallow.ID, "queue")
	if got := hops(); got != 8 {
		t.Fatalf("repeated report: hops %d", got)
	}

	// The turn ends; the next batch starts over, and keeps its own deepest.
	r.turnEnded(b.ID)
	r.onDelivered(b.ID, storeMsg(t, s, b, 2).ID, "queue")
	r.onDelivered(b.ID, storeMsg(t, s, b, 3).ID, "queue")
	if got := hops(); got != 3 {
		t.Fatalf("new turn [2, 3]: hops %d", got)
	}
	// A steer does not start a turn, so it does not start the count over.
	r.turnEnded(b.ID)
	r.onDelivered(b.ID, storeMsg(t, s, b, 1).ID, model.ModeSteer)
	if got := hops(); got != 3 {
		t.Fatalf("steer after the turn ended: hops %d", got)
	}
	// A report of a message whose turn is over already starts nothing.
	r.turnEnded(b.ID)
	r.onDelivered(b.ID, storeMsg(t, s, b, 0, func(m *model.Message) { m.State, m.SeenAt = model.MsgSeen, time.Now() }).ID, "queue")
	if got := hops(); got != 3 {
		t.Fatalf("late report: hops %d", got)
	}
	// session.status idle, routed from the node, ends the turn.
	s.route(model.Event{SessionID: b.ID, Kind: model.KindSessionStatus, Payload: model.Marshal(model.SessionStatus{Status: model.StatusIdle})})
	r.onDelivered(b.ID, storeMsg(t, s, b, 1).ID, "queue")
	if got := hops(); got != 1 {
		t.Fatalf("after idle: hops %d", got)
	}
}

// A deep steer into a running turn does not deepen it, and a person's
// message, steered or not, starts the count over. Without max_hops agents
// may keep messaging each other however deep the chain.
func TestHopsPersonAndSteer(t *testing.T) {
	s := testServer(t, Guards{MaxHops: 8})
	r := s.router
	b := addSession(t, s, "devbox", "b")
	hops := func() int { t.Helper(); return r.hopsFor(from(b)) - 1 }

	r.onDelivered(b.ID, storeMsg(t, s, b, 2).ID, "queue")
	r.onDelivered(b.ID, storeMsg(t, s, b, 7).ID, model.ModeSteer)
	if got := hops(); got != 2 {
		t.Fatalf("after a deep steer: hops %d", got)
	}
	r.onDelivered(b.ID, storeMsg(t, s, b, 6).ID, "queue")
	r.onDelivered(b.ID, storeMsg(t, s, b, 0, func(m *model.Message) { m.From = "user:jm" }).ID, model.ModeSteer)
	if got := hops(); got != 0 {
		t.Fatalf("after a person's steer: hops %d", got)
	}

	off := testServer(t, Guards{})
	c := addSession(t, off, "devbox", "c")
	off.router.onDelivered(c.ID, storeMsg(t, off, c, 500).ID, "queue")
	if _, err := off.router.send(context.Background(), from(c), model.MsgNotify, fleet.SendArgs{To: "user:jm", Text: "x"}); err != nil {
		t.Fatalf("hop 501 without max_hops: %v", err)
	}
}

// A restart rebuilds each running chain's hops from the stored messages,
// however long ago its turn started: the current turn's (entered, not yet
// seen), or else the last completed turn's.
func TestHopsSurviveRestart(t *testing.T) {
	s := testServer(t, Guards{})
	r := s.router
	busy := addSession(t, s, "devbox", "busy", func(x *model.Session) { x.Status = model.StatusActive })
	idle := addSession(t, s, "devbox", "idle")
	fresh := addSession(t, s, "devbox", "fresh")
	old := time.Now().Add(-time.Hour)
	seen := func(turn string) func(*model.Message) {
		return func(m *model.Message) {
			m.State, m.SeenAt, m.SeenTurn, m.DeliveredAt = model.MsgSeen, old, turn, old.Add(-time.Minute)
		}
	}
	storeMsg(t, s, busy, 6, seen("t1"))
	storeMsg(t, s, busy, 5) // the running turn
	storeMsg(t, s, busy, 2)
	storeMsg(t, s, busy, 7, func(m *model.Message) { m.DeliveredAt = time.Time{} }) // still queued
	storeMsg(t, s, busy, 9, func(m *model.Message) { m.State = model.MsgCancelled })
	storeMsg(t, s, idle, 4, seen("t9"))
	storeMsg(t, s, idle, 1, seen("t8"), func(m *model.Message) { m.SeenAt = old.Add(-time.Hour) })

	r.mu.Lock()
	r.inbound = map[string]trigger{} // as after a restart
	r.mu.Unlock()
	r.rebuildTriggers()
	r.mu.Lock()
	defer r.mu.Unlock()
	if tr := r.inbound[busy.ID]; tr.hops != 5 || tr.idle || len(tr.msgs) != 2 {
		t.Fatalf("running session: %+v", tr)
	}
	if tr := r.inbound[idle.ID]; tr.hops != 4 || !tr.idle || len(tr.msgs) != 1 {
		t.Fatalf("idle session: %+v", tr)
	}
	if tr, ok := r.inbound[fresh.ID]; ok {
		t.Fatalf("session without messages: %+v", tr)
	}
}

// The coordinator does not expire a message its node may be taking now:
// it looks again once that delivery has settled.
func TestNoExpiryInFlight(t *testing.T) {
	s := testServer(t, Guards{})
	r := s.router
	b := addSession(t, s, "offbox", "b")
	m := storeMsg(t, s, b, 1, func(m *model.Message) {
		m.State, m.DeliveredAt, m.ExpiresAt = model.MsgPending, time.Time{}, time.Now().Add(-time.Second)
	})
	r.mu.Lock()
	r.inflight[m.ID] = true
	r.mu.Unlock()
	r.expire(m)
	if cur, _ := s.store.Message(m.ID); cur.State != model.MsgPending {
		t.Fatalf("expired while in flight: %+v", cur)
	}
	r.mu.Lock()
	delete(r.inflight, m.ID)
	r.mu.Unlock()
	r.expire(m)
	if cur, _ := s.store.Message(m.ID); cur.State != model.MsgExpired {
		t.Fatalf("not expired once settled: %+v", cur)
	}
}

// A node's session.status the coordinator does not know is kept as error.
func TestUnknownStatusProjected(t *testing.T) {
	s := testServer(t, Guards{})
	b := addSession(t, s, "devbox", "b")
	status := func(seq int64, st string) string {
		t.Helper()
		s.project(model.Event{SessionID: b.ID, Seq: seq, Kind: model.KindSessionStatus, TS: time.Now(),
			Payload: model.Marshal(model.SessionStatus{Status: st})})
		got, _ := s.store.Session(b.ID)
		return got.Status
	}
	if got := status(1, "bogus"); got != model.StatusError {
		t.Fatalf("unknown status stored as %q", got)
	}
	if got := status(2, model.StatusActive); got != model.StatusActive {
		t.Fatalf("known status stored as %q", got)
	}
}
