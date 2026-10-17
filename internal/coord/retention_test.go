// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// pruneServer is testServer without its router's own minute tidy, which
// would prune too, concurrently with the test's calls (on the router's
// goroutine only, in a real coordinator).
func pruneServer(t *testing.T) *Server {
	s := testServer(t, Guards{})
	s.router.close()
	return s
}

// Ended sessions go after the retention, with their event logs; live
// ones, errored ones (still on their node) and recent ones stay.
func TestPruneSessions(t *testing.T) {
	s := pruneServer(t)
	now := time.Now()
	old := now.Add(-defaultSessionRetention - time.Hour)
	aged := func(status string) func(*model.Session) {
		return func(x *model.Session) {
			x.ID = ulid.New()
			x.Status = status
			x.CreatedAt, x.UpdatedAt, x.EndedAt = old, old, old
		}
	}
	stopped := addSession(t, s, "devbox", "stopped", aged(model.StatusStopped))
	gone := addSession(t, s, "devbox", "gone", aged(model.StatusGone))
	idle := addSession(t, s, "devbox", "idle", aged(model.StatusIdle))
	errored := addSession(t, s, "devbox", "errored", aged(model.StatusError))
	recent := addSession(t, s, "devbox", "recent", func(x *model.Session) {
		x.ID = ulid.New()
		x.Status = model.StatusStopped
		x.CreatedAt, x.UpdatedAt, x.EndedAt = old, old, now.Add(-time.Hour) // stopped lately
	})
	// Idle for long, then marked gone (its host removed, say): the end is
	// now, by the coordinator's clock, not its last event.
	idleThenGone := addSession(t, s, "devbox", "idle-then-gone", aged(model.StatusIdle))
	if _, err := s.store.UpdateSession(idleThenGone.ID, func(x *model.Session) { x.Status = model.StatusGone }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AppendEvents([]model.Event{{ID: ulid.New(), SessionID: stopped.ID, Seq: 1, TS: old,
		Kind: model.KindWarning, Payload: model.Marshal(model.ErrorEvent{Message: "x"})}}); err != nil {
		t.Fatal(err)
	}
	fleetCh := make(chan FleetEvent, 16)
	s.fleet[fleetCh] = struct{}{}

	s.pruneSessions(now)
	for _, sess := range []model.Session{stopped, gone} {
		if _, ok := s.store.Session(sess.ID); ok {
			t.Errorf("%s kept past the retention", sess.Path)
		}
	}
	if evs, _ := s.store.Events(stopped.ID, 0, 10); len(evs) != 0 {
		t.Errorf("events of a removed session kept: %v", evs)
	}
	for _, sess := range []model.Session{idle, errored, recent, idleThenGone} {
		if _, ok := s.store.Session(sess.ID); !ok {
			t.Errorf("%s removed", sess.Path)
		}
	}
	removed := 0
	for len(fleetCh) > 0 {
		if ev := <-fleetCh; ev.Type == "session_removed" {
			removed++
		}
	}
	if removed != 2 {
		t.Errorf("published %d session_removed, want 2", removed)
	}

	// A shorter retention in coordinator.json; a negative one keeps all.
	s.cfg.SessionRetentionDays = -1
	s.pruneSessions(now.Add(10 * 365 * 24 * time.Hour))
	if _, ok := s.store.Session(recent.ID); !ok {
		t.Fatal("a negative retention removed a session")
	}
	// One that would overflow a Duration is capped, not negative.
	s.cfg.SessionRetentionDays = 1 << 40
	s.pruneSessions(now)
	if _, ok := s.store.Session(recent.ID); !ok {
		t.Fatal("an overflowing retention removed a recent session")
	}
	s.cfg.SessionRetentionDays = 1
	s.pruneSessions(now.Add(25 * time.Hour))
	if _, ok := s.store.Session(recent.ID); ok {
		t.Fatal("a one-day retention kept a session stopped two days ago")
	}
}

// RemoveSession deletes an ended session at once, and refuses a live one.
func TestRemoveSession(t *testing.T) {
	s := pruneServer(t)
	live := addSession(t, s, "devbox", "live", func(x *model.Session) { x.ID = ulid.New() })
	done := addSession(t, s, "devbox", "done", func(x *model.Session) {
		x.ID = ulid.New()
		x.Status = model.StatusStopped
	})
	if err := s.RemoveSession(live.ID); !errors.Is(err, errLive) {
		t.Fatalf("removing a live session: %v", err)
	}
	if _, ok := s.store.Session(live.ID); !ok {
		t.Fatal("live session removed")
	}
	if err := s.RemoveSession(done.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.store.Session(done.ID); ok {
		t.Fatal("stopped session kept")
	}
	if err := s.RemoveSession(done.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removing it again: %v", err)
	}
}

// Deleting history needs admin.
func TestRemoveSessionScope(t *testing.T) {
	s := pruneServer(t)
	for _, r := range s.apiRoutes() {
		if r.pattern == "DELETE /v1/sessions/{id}" {
			if r.scope != scopeAdmin {
				t.Fatalf("DELETE /v1/sessions/{id} needs %v, want admin", r.scope)
			}
			return
		}
	}
	t.Fatal("no DELETE /v1/sessions/{id} route")
}

// Sessions above a live one in the spawn chain are kept, so its spawn depth
// cannot shrink; swk rm refuses them too.
func TestPruneKeepsAncestorsOfLive(t *testing.T) {
	s := pruneServer(t)
	now := time.Now()
	old := now.Add(-defaultSessionRetention - time.Hour)
	stoppedOld := func(parent string) func(*model.Session) {
		return func(x *model.Session) {
			x.ID = ulid.New()
			x.Status = model.StatusStopped
			x.Parent = parent
			x.CreatedAt, x.UpdatedAt, x.EndedAt = old, old, old
		}
	}
	a := addSession(t, s, "devbox", "a", stoppedOld(""))
	b := addSession(t, s, "devbox", "b", stoppedOld(a.ID))
	c := addSession(t, s, "devbox", "c", func(x *model.Session) { x.ID = ulid.New(); x.Parent = b.ID })
	s.pruneSessions(now)
	for _, sess := range []model.Session{a, b, c} {
		if _, ok := s.store.Session(sess.ID); !ok {
			t.Fatalf("%s removed while a session below it runs", sess.Path)
		}
	}
	if d := s.depth(c); d != 2 {
		t.Fatalf("depth(c) = %d, want 2", d)
	}
	if err := s.RemoveSession(a.ID); !errors.Is(err, errLive) {
		t.Fatalf("rm of an ancestor of a live session: %v", err)
	}
	// Once c stops, the chain goes.
	if _, err := s.store.UpdateSession(c.ID, func(x *model.Session) { x.Status = model.StatusStopped }); err != nil {
		t.Fatal(err)
	}
	s.pruneSessions(now)
	for _, sess := range []model.Session{a, b} {
		if _, ok := s.store.Session(sess.ID); ok {
			t.Errorf("%s kept after the session below it stopped", sess.Path)
		}
	}
}

// A removed session's node cannot bring it back: the tombstone refuses
// adopting it, and its id stays taken.
func TestRetiredNotAdopted(t *testing.T) {
	s := pruneServer(t)
	done := addSession(t, s, "devbox", "done", func(x *model.Session) {
		x.ID = ulid.New()
		x.Status = model.StatusGone
	})
	if err := s.RemoveSession(done.ID); err != nil {
		t.Fatal(err)
	}
	s.reconcile("devbox", wire.SessionState{ID: done.ID, Path: done.Path, Harness: "codex", Status: model.StatusIdle})
	if _, ok := s.store.Session(done.ID); ok {
		t.Fatal("a removed session was adopted again")
	}
	if err := s.store.CreateSession(done); !errors.Is(err, store.ErrRetired) {
		t.Fatalf("CreateSession of a removed id: %v", err)
	}
}

// A large backlog goes in batches, one per host per tidy.
func TestPruneBatches(t *testing.T) {
	s := pruneServer(t)
	old := time.Now().Add(-defaultSessionRetention - time.Hour)
	for i := range 250 {
		addSession(t, s, "devbox", fmt.Sprintf("s%d", i), func(x *model.Session) {
			x.ID = ulid.New()
			x.Status = model.StatusStopped
			x.CreatedAt, x.UpdatedAt, x.EndedAt = old, old, old
		})
	}
	s.pruneSessions(time.Now())
	if n := len(s.store.Sessions()); n != 50 {
		t.Fatalf("%d sessions after one prune, want 50", n)
	}
	s.pruneSessions(time.Now())
	if n := len(s.store.Sessions()); n != 0 {
		t.Fatalf("%d sessions after two prunes, want 0", n)
	}
}
