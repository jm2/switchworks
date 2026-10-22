// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"testing"

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
