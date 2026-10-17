// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"errors"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
)

// Ended sessions go after the retention, with their event logs; live
// ones, errored ones (still on their node) and recent ones stay.
func TestPruneSessions(t *testing.T) {
	s := testServer(t, Guards{})
	now := time.Now()
	old := now.Add(-defaultSessionRetention - time.Hour)
	aged := func(status string) func(*model.Session) {
		return func(x *model.Session) {
			x.ID = ulid.New()
			x.Status = status
			x.CreatedAt, x.UpdatedAt = old, old
		}
	}
	stopped := addSession(t, s, "devbox", "stopped", aged(model.StatusStopped))
	gone := addSession(t, s, "devbox", "gone", aged(model.StatusGone))
	idle := addSession(t, s, "devbox", "idle", aged(model.StatusIdle))
	errored := addSession(t, s, "devbox", "errored", aged(model.StatusError))
	undated := addSession(t, s, "devbox", "undated", func(x *model.Session) {
		x.ID = ulid.New()
		x.Status = model.StatusStopped
		x.CreatedAt, x.UpdatedAt = time.Time{}, time.Time{}
	})
	recent := addSession(t, s, "devbox", "recent", func(x *model.Session) {
		x.ID = ulid.New()
		x.Status = model.StatusStopped
		x.CreatedAt, x.UpdatedAt = old, now.Add(-time.Hour) // created long ago, active lately
	})
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
	for _, sess := range []model.Session{idle, errored, undated, recent} {
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
	s := testServer(t, Guards{})
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
	s := testServer(t, Guards{})
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
