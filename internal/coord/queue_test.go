// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

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
