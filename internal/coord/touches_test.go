// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
)

// A resume (registration, status and queue events with nothing new from
// the agent) keeps the session's age; its next turn moves it.
func TestResumeKeepsUpdatedAt(t *testing.T) {
	s := testServer(t, Guards{})
	old := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	sess := addSession(t, s, "devbox", "sw", func(x *model.Session) { x.UpdatedAt = old })
	now := time.Now()
	s.project(model.Event{SessionID: sess.ID, Seq: 1, Kind: model.KindSessionRegistered, TS: now,
		Payload: model.Marshal(model.SessionRegistered{Model: "m"})})
	s.project(model.Event{SessionID: sess.ID, Seq: 2, Kind: model.KindSessionStatus, TS: now,
		Payload: model.Marshal(model.SessionStatus{Status: model.StatusIdle})})
	s.project(model.Event{SessionID: sess.ID, Seq: 3, Kind: model.KindSessionQueue, TS: now,
		Payload: model.Marshal(model.SessionQueue{Queued: 0})})
	cur, _ := s.store.Session(sess.ID)
	if !cur.UpdatedAt.Equal(old) {
		t.Fatalf("a resume moved updated_at from %v to %v", old, cur.UpdatedAt)
	}
	if cur.Model != "m" || cur.ProjectedSeq != 3 {
		t.Fatalf("the resume events were not projected: model %q seq %d", cur.Model, cur.ProjectedSeq)
	}
	s.project(model.Event{SessionID: sess.ID, Seq: 4, Kind: model.KindTurnStarted, TS: now})
	cur, _ = s.store.Session(sess.ID)
	if !cur.UpdatedAt.Equal(now) {
		t.Fatalf("a turn did not move updated_at: %v", cur.UpdatedAt)
	}
}
