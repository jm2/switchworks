// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

func event(sessionID string, seq int64, kind string, p any) model.Event {
	return model.Event{ID: ulid.New(), SessionID: sessionID, Seq: seq, TS: time.Now().UTC(), Kind: kind, Payload: model.Marshal(p)}
}

func status(sessionID string, seq int64, st string) model.Event {
	return event(sessionID, seq, model.KindSessionStatus, model.SessionStatus{Status: st})
}

// putAsk stores an ask from one session to another, already delivered.
func putAsk(t *testing.T, s *Server, from, to model.Session) model.Message {
	t.Helper()
	now := time.Now().UTC()
	m := model.Message{ID: ulid.New(), Kind: model.MsgAsk, From: from.Path, FromSession: from.ID, To: to.Path,
		ToSession: to.ID, ToPath: to.Path, Text: "6x7?", State: model.MsgDelivered, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), DeliveredAt: now}
	if err := s.store.PutMessage(m); err != nil {
		t.Fatal(err)
	}
	return m
}

// noticesTo returns the system notices queued for a session.
func noticesTo(s *Server, sess model.Session) []model.Message {
	var out []model.Message
	for _, m := range s.store.Messages(store.MessageFilter{Session: sess.ID}) {
		if m.Kind == model.MsgSystem && m.ToSession == sess.ID {
			out = append(out, m)
		}
	}
	return out
}

func msgState(s *Server, id string) string {
	m, _ := s.store.Message(id)
	return m.State
}

// Events stored but not yet in the snapshot when the coordinator stopped
// are projected again at startup, once.
func TestReplayProjections(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t, Guards{})
	s.store = st
	asker := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.ID = ulid.New() })
	sess := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	ask := putAsk(t, s, asker, sess)
	stored, err := st.AppendEvents([]model.Event{status(sess.ID, 1, model.StatusActive)})
	if err != nil {
		t.Fatal(err)
	}
	s.project(stored[0])
	// The crash: stored and acknowledged, never projected.
	if _, err := st.AppendEvents([]model.Event{
		event(sess.ID, 2, model.KindUsage, model.UsageEvent{Last: tok(100, 10, 110), Total: tok(100, 10, 110)}),
		status(sess.ID, 3, model.StatusStopped),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // a second start changes nothing
		st, err = store.OpenFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		s2 := testServer(t, Guards{})
		s2.store = st
		s2.replayProjections()
		got, _ := st.Session(sess.ID)
		if got.Status != model.StatusStopped || got.ProjectedSeq != 3 || got.Usage == nil || got.Usage.TotalTokens != 110 {
			t.Fatalf("start %d: %+v (usage %+v)", i, got, got.Usage)
		}
		if d := st.DailyTokens(today()); d != 110 {
			t.Fatalf("start %d: daily tokens %d", i, d)
		}
		// The stopped session's unanswered ask fails, and its sender hears.
		if m, _ := st.Message(ask.ID); m.State != model.MsgFailed {
			t.Fatalf("start %d: ask %s", i, m.State)
		}
		if n := noticesTo(s2, asker); len(n) != 1 {
			t.Fatalf("start %d: %d notices", i, len(n))
		}
		s2.router.close()
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A spawn in flight when the coordinator stopped (starting, no harness, no
// events) does not hold its path forever.
func TestPurgeStartingSpawns(t *testing.T) {
	s := testServer(t, Guards{})
	orphan := addSession(t, s, "antec", "sw", func(x *model.Session) { x.ID, x.Status = ulid.New(), model.StatusStarting })
	started := addSession(t, s, "antec", "ran", func(x *model.Session) { x.ID, x.Status = ulid.New(), model.StatusStarting })
	if _, err := s.store.AppendEvents([]model.Event{status(started.ID, 1, model.StatusStarting)}); err != nil {
		t.Fatal(err)
	}
	s.purgeFailedSpawns()
	if _, ok := s.store.Session(orphan.ID); ok {
		t.Fatal("orphaned spawn kept")
	}
	if _, ok := s.store.Session(started.ID); !ok {
		t.Fatal("session with events removed")
	}
}

// A hello marks gone the host's live sessions its node no longer has,
// except ones spawned moments ago, and acknowledges everything of sessions
// it does not adopt.
func TestHelloMarksUnreportedGone(t *testing.T) {
	s := testServer(t, Guards{})
	old := time.Now().Add(-time.Hour)
	lead := addSession(t, s, "other", "lead", func(x *model.Session) { x.ID = ulid.New() })
	stale := addSession(t, s, "devbox", "stale", func(x *model.Session) { x.ID, x.CreatedAt = ulid.New(), old })
	ask := putAsk(t, s, lead, stale)
	fresh := addSession(t, s, "devbox", "fresh", func(x *model.Session) { x.ID, x.Status = ulid.New(), model.StatusStarting })
	kept := addSession(t, s, "devbox", "kept", func(x *model.Session) { x.ID, x.CreatedAt = ulid.New(), old })
	elsewhere := addSession(t, s, "other", "x", func(x *model.Session) { x.ID, x.CreatedAt = ulid.New(), old })
	// The node has a session on the stale one's path the coordinator lost,
	// and one it will not adopt.
	adopt := wire.SessionState{ID: ulid.New(), Path: "devbox/stale", Harness: "codex", Status: model.StatusIdle, LastSeq: 4}
	bad := wire.SessionState{ID: ulid.New(), Path: "elsewhere/x", Harness: "codex", Status: model.StatusStopped, LastSeq: 9}
	acked := s.greet("devbox", wire.Hello{Host: "devbox", Sessions: []wire.SessionState{
		{ID: kept.ID, Path: kept.Path, Harness: "codex", Status: model.StatusIdle}, adopt, bad}})

	for id, want := range map[string]string{stale.ID: model.StatusGone, fresh.ID: model.StatusStarting,
		kept.ID: model.StatusIdle, elsewhere.ID: model.StatusIdle, adopt.ID: model.StatusIdle} {
		if got, _ := s.store.Session(id); got.Status != want {
			t.Errorf("%s: %q, want %q", got.Path, got.Status, want)
		}
	}
	if msgState(s, ask.ID) != model.MsgFailed {
		t.Error("ask to the gone session not failed")
	}
	if acked[bad.ID] != 9 || acked[adopt.ID] != 0 || acked[kept.ID] != 0 {
		t.Errorf("acks %v", acked)
	}
	// A hello cut at maxHelloSessions does not say what the node lacks.
	many := make([]wire.SessionState, maxHelloSessions+1)
	for i := range many {
		many[i] = wire.SessionState{ID: "x"}
	}
	s.greet("devbox", wire.Hello{Host: "devbox", Sessions: many})
	if got, _ := s.store.Session(kept.ID); got.Status != model.StatusIdle {
		t.Errorf("truncated hello marked %s %s", got.Path, got.Status)
	}
}

// Events a coordinator refuses are acknowledged, so the node drops them
// instead of keeping and resending them forever.
func TestIngestAcksRefused(t *testing.T) {
	s := testServer(t, Guards{})
	mine := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	theirs := addSession(t, s, "other", "b", func(x *model.Session) { x.ID = ulid.New() })
	unknown := ulid.New()
	badKind := event(mine.ID, 2, "Bad Kind", nil)
	acked, err := s.ingest("devbox", []model.Event{status(mine.ID, 1, model.StatusIdle), badKind,
		status(unknown, 5, model.StatusIdle), status(theirs.ID, 7, model.StatusIdle)})
	if err != nil {
		t.Fatal(err)
	}
	if acked[mine.ID] != 2 || acked[unknown] != 5 || acked[theirs.ID] != 7 {
		t.Fatalf("acks %v", acked)
	}
	if s.store.LastSeq(theirs.ID) != 0 || s.store.LastSeq(unknown) != 0 {
		t.Fatal("refused events stored")
	}
}

// A host's batches are stored and projected one at a time, so a replaced
// connection's last batch and the resend cannot interleave.
func TestIngestSerializedPerHost(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "devbox", "a", func(x *model.Session) { x.ID = ulid.New() })
	unlock := s.lockIngest("devbox")
	done := make(chan struct{})
	go func() {
		_, _ = s.ingest("devbox", []model.Event{status(sess.ID, 1, model.StatusIdle)})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("ingest ran while the host's ingest lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-done

	// The old connection's batch and the resend, side by side: the usage
	// counts once whatever the order.
	var evs []model.Event
	for i := int64(2); i <= 41; i++ {
		evs = append(evs, event(sess.ID, i, model.KindUsage, model.UsageEvent{Total: tok(i*100, i, i*101)}))
	}
	var wg sync.WaitGroup
	for _, batch := range [][]model.Event{evs[:20], evs} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.ingest("devbox", batch)
		}()
	}
	wg.Wait()
	if got := sessionUsage(t, s, sess.ID); got.TotalTokens != 41*101 {
		t.Fatalf("usage %+v, want total %d", got, 41*101)
	}
}

// message.failed (the harness did not take a delivered message) fails it
// and tells the sender; a node cannot fail another session's messages.
func TestMessageFailedEvent(t *testing.T) {
	s := testServer(t, Guards{})
	lead := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.ID = ulid.New() })
	worker := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	bystander := addSession(t, s, "devbox", "other", func(x *model.Session) { x.ID = ulid.New() })
	ask := putAsk(t, s, lead, worker)
	if _, err := s.ingest("devbox", []model.Event{event(bystander.ID, 1, model.KindMessageFailed,
		model.MessageEvent{MessageID: ask.ID, Error: "nope"})}); err != nil {
		t.Fatal(err)
	}
	if msgState(s, ask.ID) != model.MsgDelivered {
		t.Fatal("another session's message.failed failed the ask")
	}
	if _, err := s.ingest("devbox", []model.Event{event(worker.ID, 1, model.KindMessageFailed,
		model.MessageEvent{MessageID: ask.ID, Error: "turn/start: boom"})}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.store.Message(ask.ID); m.State != model.MsgFailed || !strings.Contains(m.Error, "boom") {
		t.Fatalf("ask %s %q", m.State, m.Error)
	}
	if n := noticesTo(s, lead); len(n) != 1 || !strings.Contains(n[0].Text, "boom") {
		t.Fatalf("notices %+v", n)
	}
	res, err := s.router.inbox(context.Background(), from(lead), fleet.InboxArgs{Ticket: ask.ID})
	if err != nil || res.Ticket.Status != fleet.AskFailed {
		t.Fatalf("ticket %+v %v", res.Ticket, err)
	}
}

// An ask whose target stops is failed, and a fleet_ask waiting on it
// returns at once.
func TestStoppedTargetFailsAsks(t *testing.T) {
	s := testServer(t, Guards{})
	lead := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.ID = ulid.New() })
	worker := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	ask := putAsk(t, s, lead, worker)
	got := make(chan *model.Message, 1)
	go func() { got <- s.router.waitReply(context.Background(), ask.ID, time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	if _, err := s.ingest("devbox", []model.Event{status(worker.ID, 1, model.StatusStopped)}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m == nil || m.ID != ask.ID || m.State != model.MsgFailed {
			t.Fatalf("waiter got %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter still waiting")
	}
	if n := noticesTo(s, lead); len(n) != 1 || !strings.Contains(n[0].Text, "will not be answered") {
		t.Fatalf("notices %+v", n)
	}
}

// A failed reply sends its sender no notice (it would be a notice about a
// reply).
func TestNoNoticeAboutReplies(t *testing.T) {
	s := testServer(t, Guards{})
	worker := addSession(t, s, "devbox", "worker", func(x *model.Session) { x.ID = ulid.New() })
	rep := model.Message{ID: ulid.New(), Kind: model.MsgReply, From: worker.Path, FromSession: worker.ID,
		To: "devbox/lead", State: model.MsgPending, CreatedAt: time.Now()}
	s.router.fail(rep, "the asker no longer exists")
	if n := noticesTo(s, worker); len(n) != 0 {
		t.Fatalf("notices about a reply: %+v", n)
	}
}

// A timed-out spawn that starts after all, once another session has taken
// its path, does not come back: one path, one live session.
func TestLateSpawnDoesNotRevive(t *testing.T) {
	s := testServer(t, Guards{})
	late := addSession(t, s, "devbox", "sw", func(x *model.Session) { x.ID, x.Status = ulid.New(), model.StatusError })
	now := addSession(t, s, "devbox", "sw", func(x *model.Session) { x.ID = ulid.New() })
	if _, err := s.ingest("devbox", []model.Event{
		event(late.ID, 1, model.KindSessionRegistered, model.SessionRegistered{NativeRef: model.NativeRef{Harness: "codex", ID: "t1"}}),
		status(late.ID, 2, model.StatusIdle),
	}); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait() // the stop it sends (the host is offline here)
	if got, _ := s.store.Session(late.ID); got.Status != model.StatusGone {
		t.Fatalf("late session %s", got.Status)
	}
	if got, _ := s.store.SessionByPath("devbox/sw"); got.ID != now.ID {
		t.Fatalf("path held by %s", got.ID)
	}
	// With its path free, it does come back.
	alone := addSession(t, s, "devbox", "solo", func(x *model.Session) { x.ID, x.Status = ulid.New(), model.StatusError })
	if _, err := s.ingest("devbox", []model.Event{status(alone.ID, 1, model.StatusIdle)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.store.Session(alone.ID); got.Status != model.StatusIdle {
		t.Fatalf("revival refused without a conflict: %s", got.Status)
	}
}

// A closed router starts no more background deliveries (the store they
// would write to is about to close).
func TestRouterCloseStopsDeliveries(t *testing.T) {
	s := testServer(t, Guards{})
	s.router.close()
	m := model.Message{ID: ulid.New(), Kind: model.MsgNotify, ToUser: "user:jm", State: model.MsgPending, CreatedAt: time.Now()}
	if err := s.store.PutMessage(m); err != nil {
		t.Fatal(err)
	}
	s.router.deliverLater(m)
	s.router.bg.Wait()
	if msgState(s, m.ID) != model.MsgPending {
		t.Fatal("delivered after close")
	}
}
