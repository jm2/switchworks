// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

func tok(in, out, total int64) model.Usage {
	return model.Usage{InputTokens: in, OutputTokens: out, TotalTokens: total}
}

// feed stores usage events for sess and projects them, as ingest does.
func feed(t *testing.T, s *Server, sessionID string, evs ...model.UsageEvent) {
	t.Helper()
	for _, p := range evs {
		ev := model.Event{ID: ulid.New(), SessionID: sessionID, Seq: s.store.LastSeq(sessionID) + 1,
			TS: time.Now().UTC(), Kind: model.KindUsage, Payload: model.Marshal(p)}
		stored, err := s.store.AppendEvents([]model.Event{ev})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range stored {
			s.project(e)
		}
	}
}

func sessionUsage(t *testing.T, s *Server, id string) model.Usage {
	t.Helper()
	sess, ok := s.store.Session(id)
	if !ok || sess.Usage == nil {
		t.Fatalf("session %s has no usage", id)
	}
	return *sess.Usage
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

// A session's usage survives its adapter's Total starting over (acpad and
// agyad count from their agent process, which a node restart replaces),
// including across a coordinator restart.
func TestUsageSurvivesAdapterReset(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "h1", "goose", func(x *model.Session) { x.ID, x.Harness = ulid.New(), "goose" })

	t1, t2, t3 := tok(8484, 57, 8541), tok(8788, 22, 8810), tok(8903, 137, 9040)
	feed(t, s, sess.ID,
		model.UsageEvent{Last: t1, Total: t1},
		model.UsageEvent{Last: t2, Total: tok(17272, 79, 17351)},
		model.UsageEvent{Last: t3, Total: tok(26175, 216, 26391)})
	if got := sessionUsage(t, s, sess.ID); got != tok(26175, 216, 26391) {
		t.Fatalf("before restart: %+v", got)
	}
	// The node restarts: the new agent process reports from zero.
	t4 := tok(9121, 3, 9124)
	feed(t, s, sess.ID, model.UsageEvent{Last: t4, Total: t4})
	want := tok(35296, 219, 35515)
	if got := sessionUsage(t, s, sess.ID); got != want {
		t.Fatalf("after adapter restart: got %+v, want %+v", got, want)
	}

	// A coordinator restart forgets the adapter's last Total; it is read
	// back from the session's log, so the next turn adds only its own.
	s2 := testServer(t, Guards{})
	s2.store = s.store
	t5 := tok(9300, 40, 9340)
	feed(t, s2, sess.ID, model.UsageEvent{Last: t5, Total: tok(18421, 43, 18464)})
	want = tok(44596, 259, 44855)
	if got := sessionUsage(t, s2, sess.ID); got != want {
		t.Fatalf("after coordinator restart: got %+v, want %+v", got, want)
	}
	if d := s.store.DailyTokens(today()); d != want.TotalTokens {
		t.Fatalf("daily tokens %d, want %d", d, want.TotalTokens)
	}
}

// A new process whose first turn outgrows the old process's whole Total
// is still a restart: its Total equals its Last.
func TestUsageResetThatGrew(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "h1", "agy", func(x *model.Session) { x.ID, x.Harness = ulid.New(), "agy" })
	feed(t, s, sess.ID,
		model.UsageEvent{Last: tok(100, 10, 110), Total: tok(100, 10, 110)},
		model.UsageEvent{Last: tok(500, 50, 550), Total: tok(500, 50, 550)})
	if got := sessionUsage(t, s, sess.ID); got != tok(600, 60, 660) {
		t.Fatalf("usage %+v", got)
	}
}

// Codex's Total spans the thread (and resumes); a repeated report adds
// nothing, and Last (one model request) is not summed.
func TestUsageCodexTotals(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "h1", "codex", func(x *model.Session) { x.ID = ulid.New() })
	feed(t, s, sess.ID,
		model.UsageEvent{Last: tok(1000, 10, 1010), Total: tok(1000, 10, 1010)},
		model.UsageEvent{Last: tok(1200, 20, 1220), Total: tok(2200, 30, 2230)},
		model.UsageEvent{Last: tok(1200, 20, 1220), Total: tok(2200, 30, 2230)}, // repeat
		model.UsageEvent{Last: tok(1300, 5, 1305), Total: tok(3500, 35, 3535)})
	if got := sessionUsage(t, s, sess.ID); got != tok(3500, 35, 3535) {
		t.Fatalf("usage %+v", got)
	}
	if d := s.store.DailyTokens(today()); d != 3535 {
		t.Fatalf("daily tokens %d", d)
	}
}
