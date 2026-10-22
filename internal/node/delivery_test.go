// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// deliverCmd sends m and returns the node's result, refused or not.
func deliverCmd(n *Node, s *session, m wire.MessageCmd) wire.Result {
	if m.ID == "" {
		m.ID = ulid.New()
	}
	return n.handleCmd(wire.Cmd{Op: wire.OpDeliver, Session: s.rec.ID, Message: &m})
}

// kindIndex is where the first event of kind is in the session's log, or -1.
func (s *session) kindIndex(kind string) int {
	for i, k := range s.eventKinds() {
		if k == kind {
			return i
		}
	}
	return -1
}

// A full queue refuses more, telling the sender; the owner keeps a share
// of it others cannot fill, and a refused message is not taken as seen.
func TestQueueLimits(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	agentShare := int(share(queuedMsg{From: "devbox/b"}, maxQueueMessages))
	s.mu.Lock()
	for i := 0; i < agentShare; i++ {
		s.rec.Queue = append(s.rec.Queue, queuedMsg{ID: ulid.New(), From: "devbox/b", Text: "x"})
	}
	s.mu.Unlock()
	refused := ulid.New()
	res := deliverCmd(n, s, wire.MessageCmd{ID: refused, From: "devbox/c", Text: "one more"})
	if res.OK || !strings.Contains(res.Error, "is full") {
		t.Fatalf("an agent's message to a full queue: %+v", res)
	}
	if res := deliverCmd(n, s, wire.MessageCmd{From: "user:cli", Via: "token:laptop", Text: "a person"}); res.OK {
		t.Fatal("another person's message used the owner's share")
	}
	if res := deliverCmd(n, s, wire.MessageCmd{From: "user:web", Owner: true, Text: "the owner"}); !res.OK {
		t.Fatalf("the owner's message: %s", res.Error)
	}
	// Room again: the refused message is taken when it comes back.
	s.mu.Lock()
	s.rec.Queue = s.rec.Queue[:10]
	s.mu.Unlock()
	if dr := send(t, n, s, wire.MessageCmd{ID: refused, From: "devbox/c", Text: "one more"}); dr.Duplicate {
		t.Fatal("a refused message counted as seen")
	}

	// Text counts too.
	big := queuedMsg{From: "devbox/b", Text: strings.Repeat("y", maxQueueText/4)}
	q := &session{rec: record{Path: "devbox/q", Queue: []queuedMsg{big, big}}}
	if err := q.roomLocked(big); err != nil {
		t.Fatalf("three quarters: %v", err)
	}
	q.rec.Queue = append(q.rec.Queue, big)
	if err := q.roomLocked(big); err == nil {
		t.Fatal("an agent's message past three quarters of the text limit")
	}
	if err := q.roomLocked(queuedMsg{From: "user:web", Owner: true, Text: big.Text}); err != nil {
		t.Fatalf("the owner's: %v", err)
	}
}

// Queued images are bounded per node, across its sessions, with the same
// share for the owner.
func TestQueuedImageLimit(t *testing.T) {
	n, _ := queueNode(t, &queueHandle{status: model.StatusActive})
	other := &session{n: n, rec: record{ID: ulid.New(), Path: "devbox/z"}}
	n.mu.Lock()
	n.sessions[other.rec.ID] = other
	n.mu.Unlock()
	t.Cleanup(func() {
		n.mu.Lock()
		delete(n.sessions, other.rec.ID)
		n.mu.Unlock()
	})
	other.rec.Queue = []queuedMsg{{ID: ulid.New(), From: "devbox/b", Attachments: []model.Attachment{{Size: maxQueuedImageBytes * 3 / 4}}}}
	img := []model.Attachment{{Size: 1 << 20}}
	if err := n.imageRoom(queuedMsg{From: "devbox/c", Attachments: img}); err == nil {
		t.Fatal("an agent's image past the node's limit")
	}
	if err := n.imageRoom(queuedMsg{From: "user:web", Owner: true, Attachments: img}); err != nil {
		t.Fatalf("the owner's image: %v", err)
	}
	if err := n.imageRoom(queuedMsg{From: "devbox/c"}); err != nil {
		t.Fatalf("no images: %v", err)
	}
}

// The sweep drops expired messages even while the session is busy, as in
// a long turn or an approval wait.
func TestSweepExpiresWhileBusy(t *testing.T) {
	h := &queueHandle{status: model.StatusWaitingApproval}
	n, s := queueNode(t, h)
	stale, live := ulid.New(), ulid.New()
	send(t, n, s, wire.MessageCmd{ID: stale, From: "devbox/b", Text: "stale", ExpiresAt: time.Now().Add(10 * time.Millisecond)})
	send(t, n, s, wire.MessageCmd{ID: live, From: "devbox/b", Text: "live", ExpiresAt: time.Now().Add(time.Hour)})
	time.Sleep(20 * time.Millisecond)
	n.sweepQueues(time.Now())
	if got := queueIDs(t, n, s); len(got) != 1 || got[0] != live {
		t.Fatalf("queue %v", got)
	}
	if ex := s.events(model.KindMessageExpired); len(ex) != 1 || !strings.Contains(ex[0], stale) {
		t.Fatalf("message.expired events %v", ex)
	}
}

// Stopping a session fails its queue, so senders hear, and the node forgets
// it once the coordinator has its events: no record, spool or second
// spawn under its id.
func TestStopFailsQueueAndForgets(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	a, b := ulid.New(), ulid.New()
	send(t, n, s, wire.MessageCmd{ID: a, From: "devbox/b", Text: "one"})
	send(t, n, s, wire.MessageCmd{ID: b, From: "user:web", Owner: true, Text: "two"})
	if res := n.handleCmd(wire.Cmd{Op: wire.OpStop, Session: s.rec.ID}); !res.OK {
		t.Fatalf("stop: %s", res.Error)
	}
	failed := strings.Join(s.events(model.KindMessageFailed), "\n")
	if !strings.Contains(failed, a) || !strings.Contains(failed, b) || s.queueLen() != 0 {
		t.Fatalf("failed %s; queue %d", failed, s.queueLen())
	}
	id := s.rec.ID
	n.forgetStopped()
	if _, err := n.session(id); err != nil {
		t.Fatal("forgotten with events the coordinator does not have")
	}
	s.mu.Lock()
	seq := s.seq
	s.mu.Unlock()
	n.applyAcks(map[string]int64{id: seq}, false)
	if _, err := n.session(id); !errors.Is(err, errForgotten) {
		t.Fatalf("not forgotten: %v", err)
	}
	if _, err := os.Stat(filepath.Join(n.dir, "spool", id+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("spool kept: %v", err)
	}
	st, _ := os.ReadFile(filepath.Join(n.dir, "state.json"))
	if strings.Contains(string(st), id) {
		t.Fatalf("state.json keeps it: %s", st)
	}
	if res := n.handleCmd(wire.Cmd{Op: wire.OpStop, Session: id}); !res.OK {
		t.Fatalf("stop again: %s", res.Error)
	}
	if res := n.handleCmd(wire.Cmd{Op: wire.OpDeliver, Session: id, Message: &wire.MessageCmd{ID: ulid.New(), From: "devbox/b", Text: "x"}}); res.OK {
		t.Fatal("delivered to a forgotten session")
	}
	if _, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: id, Path: "devbox/a"},
		Spec: model.SpawnSpec{Harness: "fake", Cwd: t.TempDir()}}); err == nil {
		t.Fatal("spawned again under a forgotten id")
	}
}

// A message the node cannot save is refused, not kept in memory only, and
// can be sent again once it can.
func TestDeliverFailsWhenUnsaved(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	tmp := filepath.Join(n.dir, "state.json.tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil { // saveState cannot write it
		t.Fatal(err)
	}
	id := ulid.New()
	if res := deliverCmd(n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "x"}); res.OK || !strings.Contains(res.Error, "save node state") {
		t.Fatalf("deliver: %+v", res)
	}
	if s.queueLen() != 0 || len(s.events(model.KindMessageQueued)) != 0 {
		t.Fatal("kept although unsaved")
	}
	_ = os.Remove(tmp)
	if dr := send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "x"}); dr.Duplicate || s.queueLen() != 1 {
		t.Fatalf("sent again: %+v, queue %d", dr, s.queueLen())
	}
}

// M11: a batch the harness refuses is retried one message at a time, and
// only the message that fails alone is reported failed.
func TestBatchFailureRetriesEach(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	h.hook = func(in adapter.Input, _ adapter.DeliveryMode) error {
		if strings.Contains(in.Text, "poison") {
			return errors.New("harness refused the input")
		}
		return nil
	}
	n, s := queueNode(t, h)
	ids := []string{ulid.New(), ulid.New(), ulid.New()}
	send(t, n, s, wire.MessageCmd{ID: ids[0], From: "devbox/b", Text: "first"})
	send(t, n, s, wire.MessageCmd{ID: ids[1], From: "devbox/c", Text: "poison"})
	send(t, n, s, wire.MessageCmd{ID: ids[2], From: "user:web", Owner: true, Text: "the owner's"})
	h.setStatus(model.StatusIdle)
	eventually(t, "the queue worked through", func() bool { return s.queueLen() == 0 && len(s.events(model.KindMessageFailed)) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := h.deliveries(); len(got) != 2 {
		t.Fatalf("%d deliveries, want the two good messages alone: %+v", len(got), got)
	}
	if f := s.events(model.KindMessageFailed); !strings.Contains(f[0], ids[1]) {
		t.Fatalf("failed %v", f)
	}
	delivered := strings.Join(s.events(model.KindMessageDelivered), "\n")
	if !strings.Contains(delivered, ids[0]) || !strings.Contains(delivered, ids[2]) || strings.Contains(delivered, ids[1]) {
		t.Fatalf("delivered %s", delivered)
	}
}

// A message whose stored image is gone fails alone; the rest of its batch
// goes.
func TestMissingImageFailsAlone(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, images: true}
	n, s := queueNode(t, h)
	withImage, plain := ulid.New(), ulid.New()
	send(t, n, s, wire.MessageCmd{ID: withImage, From: "devbox/b", Text: "look", Attachments: []model.Attachment{{Name: "a.png", Data: testPNG(t, 2)}}})
	send(t, n, s, wire.MessageCmd{ID: plain, From: "devbox/c", Text: "plain"})
	n.dropImages(s.rec.ID)
	h.setStatus(model.StatusIdle)
	eventually(t, "the queue worked through", func() bool { return s.queueLen() == 0 && len(h.deliveries()) == 1 })
	if f := s.events(model.KindMessageFailed); len(f) != 1 || !strings.Contains(f[0], withImage) {
		t.Fatalf("failed %v", f)
	}
	if got := h.deliveries()[0].in.Text; !strings.Contains(got, "plain") || strings.Contains(got, "look") {
		t.Fatalf("delivered %q", got)
	}
}

// A turn the harness does not answer in time is neither resent nor
// failed; if it does start, its messages are reported delivered then.
func TestDeliverTimeoutIsUnknown(t *testing.T) {
	defer func(d time.Duration) { turnTimeout = d }(turnTimeout)
	turnTimeout = 30 * time.Millisecond
	gate := make(chan struct{})
	h := &queueHandle{status: model.StatusActive, gate: gate}
	n, s := queueNode(t, h)
	id := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "slow"})
	h.setStatus(model.StatusIdle)
	eventually(t, "the attempt given up", func() bool { return s.queueLen() == 0 })
	time.Sleep(50 * time.Millisecond) // no second attempt
	close(gate)
	if len(h.deliveries()) != 0 || len(s.events(model.KindMessageFailed)) != 0 || len(s.events(model.KindMessageDelivered)) != 0 {
		t.Fatalf("deliveries %d, events %v", len(h.deliveries()), s.eventKinds())
	}
	if w := s.events(model.KindWarning); len(w) != 1 || !strings.Contains(w[0], "may or may not") {
		t.Fatalf("warnings %v", w)
	}
	// The harness started the turn after all.
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "t1"}})
	if d := s.events(model.KindMessageDelivered); len(d) != 1 || !strings.Contains(d[0], id) {
		t.Fatalf("delivered %v", d)
	}
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "t1", Status: "completed"}})
	if seen := s.events(model.KindMessageSeen); len(seen) != 1 || !strings.Contains(seen[0], id) {
		t.Fatalf("seen %v", seen)
	}
}

// L1: message.delivered comes once the batch is in: before the turn's own
// events when the turn starts during Deliver.
func TestDeliveredWhenTurnStarts(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	h.hook = func(_ adapter.Input, mode adapter.DeliveryMode) error {
		if mode == adapter.DeliverQueue {
			h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "t1"}})
		}
		return nil
	}
	n, s := queueNode(t, h)
	id := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "go"})
	if s.kindIndex(model.KindMessageDelivered) >= 0 {
		t.Fatal("delivered while queued")
	}
	h.setStatus(model.StatusIdle)
	eventually(t, "the turn", func() bool { return s.queueLen() == 0 && len(h.deliveries()) == 1 })
	if d := s.events(model.KindMessageDelivered); len(d) != 1 {
		t.Fatalf("delivered %v", d)
	}
	if s.kindIndex(model.KindMessageDelivered) > s.kindIndex(model.KindTurnStarted) {
		t.Fatalf("delivered after the turn started: %v", s.eventKinds())
	}
}

// L2: a steer is credited to the turn running when it was sent, even when
// that turn completes, and another starts, before the steer's answer.
func TestSteerSeenByItsTurn(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, steer: true}
	n, s := queueNode(t, h)
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "t1"}})
	h.hook = func(_ adapter.Input, mode adapter.DeliveryMode) error {
		if mode == adapter.DeliverSteer {
			h.sink(adapter.Event{Kind: model.KindItemCompleted, Payload: model.Item{TurnID: "t1", ItemID: "i1", ItemType: "agentMessage", Text: "ok"}})
			h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "t1", Status: "completed"}})
			h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "t2"}})
		}
		return nil
	}
	id := ulid.New()
	if dr := send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "steer me", Mode: model.ModeSteer}); dr.Mode != model.ModeSteer {
		t.Fatalf("mode %s", dr.Mode)
	}
	var seen []model.MessageSeen
	for _, p := range s.events(model.KindMessageSeen) {
		var m model.MessageSeen
		_ = json.Unmarshal([]byte(p), &m)
		seen = append(seen, m)
	}
	if len(seen) != 1 || seen[0].MessageID != id || seen[0].TurnID != "t1" || seen[0].ItemID != "i1" {
		t.Fatalf("seen %+v", seen)
	}
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "t2", Status: "completed"}})
	if got := s.events(model.KindMessageSeen); len(got) != 1 {
		t.Fatalf("credited to the next turn too: %v", got)
	}
	if s.kindIndex(model.KindMessageDelivered) > s.kindIndex(model.KindMessageSeen) {
		t.Fatalf("seen before delivered: %v", s.eventKinds())
	}
}

// L4, L7: a message being promoted stays queued, marked as being sent, so
// its image survives a collection and a save keeps it; the batch a new
// turn carries is saved as being sent before the harness gets it.
func TestSendingIsKeptAndSaved(t *testing.T) {
	gate := make(chan struct{})
	h := &queueHandle{status: model.StatusActive, steer: true, images: true, gate: gate}
	n, s := queueNode(t, h)
	id := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "look", Mode: model.ModeQueue,
		Attachments: []model.Attachment{{Name: "a.png", Data: testPNG(t, 2)}}})
	s.mu.Lock()
	sha := s.rec.Queue[0].Attachments[0].SHA256
	s.mu.Unlock()
	done := make(chan wire.Result, 1)
	go func() {
		done <- n.handleCmd(wire.Cmd{Op: wire.OpPromote, Session: s.rec.ID, Queued: &wire.QueuedCmd{MessageID: id, Mode: model.ModeSteer}})
	}()
	eventually(t, "the steer in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.steering
	})
	n.gcImages(s)
	if _, err := os.Stat(filepath.Join(n.attachDir(s.rec.ID), sha)); err != nil {
		t.Fatalf("image of the message being promoted removed: %v", err)
	}
	if st, _ := os.ReadFile(filepath.Join(n.dir, "state.json")); !strings.Contains(string(st), `"sending": true`) {
		t.Fatalf("state.json: %s", st)
	}
	if res := n.handleCmd(wire.Cmd{Op: wire.OpCancel, Session: s.rec.ID, Queued: &wire.QueuedCmd{MessageID: id}}); res.OK {
		t.Fatal("cancelled while being sent")
	}
	close(gate)
	if res := <-done; !res.OK || !strings.Contains(string(res.Data), model.ModeSteer) {
		t.Fatalf("promote: %+v", res)
	}
	if s.queueLen() != 0 {
		t.Fatal("still queued")
	}
	if _, err := os.Stat(filepath.Join(n.attachDir(s.rec.ID), sha)); !os.IsNotExist(err) {
		t.Fatalf("image kept after delivery: %v", err)
	}

	// A new turn: saved as being sent before Deliver.
	h2 := &queueHandle{status: model.StatusActive}
	n2, s2 := queueNode(t, h2)
	h2.hook = func(adapter.Input, adapter.DeliveryMode) error {
		if st, _ := os.ReadFile(filepath.Join(n2.dir, "state.json")); !strings.Contains(string(st), `"sending": true`) {
			return errors.New("not saved as being sent")
		}
		return nil
	}
	send(t, n2, s2, wire.MessageCmd{From: "devbox/b", Text: "turn"})
	h2.setStatus(model.StatusIdle)
	eventually(t, "the turn", func() bool { return s2.queueLen() == 0 })
	if f := s2.events(model.KindMessageFailed); len(f) != 0 || len(h2.deliveries()) != 1 {
		t.Fatalf("failed %v", f)
	}
}

// L7: after a restart, a message left marked as being sent is reported
// failed rather than sent again, and ids an older node kept are kept on.
func TestRecoverAfterCrash(t *testing.T) {
	n := &Node{dir: t.TempDir(), logf: t.Logf, sessions: map[string]*session{}}
	sent, waiting, old := ulid.New(), ulid.New(), ulid.New()
	s := &session{n: n, rec: record{ID: ulid.New(), Path: "devbox/a", Seen: []string{old},
		Queue: []queuedMsg{{ID: sent, From: "devbox/b", Sending: true}, {ID: waiting, From: "devbox/c"}}}}
	now := time.Now()
	if !s.recover(now) {
		t.Fatal("unchanged")
	}
	if len(s.rec.Queue) != 1 || s.rec.Queue[0].ID != waiting {
		t.Fatalf("queue %+v", s.rec.Queue)
	}
	if f := s.events(model.KindMessageFailed); len(f) != 1 || !strings.Contains(f[0], sent) || !strings.Contains(f[0], "restarted") {
		t.Fatalf("failed %v", f)
	}
	if s.rec.Seen != nil || !s.acceptedLocked(old, now) {
		t.Fatalf("accepted %+v", s.rec.Accepted)
	}
	if s.acceptedLocked(old, now.Add(seenFor+time.Minute)) {
		t.Fatal("kept past its time")
	}
}

// Accepted ids are kept until the coordinator can no longer redeliver: an
// hour past the message's expiry, or seenFor without one.
func TestAcceptedUntil(t *testing.T) {
	s := &session{}
	now := time.Now()
	a, b := ulid.New(), ulid.New()
	s.acceptLocked(a, now.Add(48*time.Hour), now)
	s.acceptLocked(b, time.Time{}, now)
	if !s.acceptedLocked(b, now.Add(24*time.Hour)) || s.acceptedLocked(b, now.Add(seenFor+time.Second)) {
		t.Fatal("no-expiry id kept for the wrong time")
	}
	if !s.acceptedLocked(a, now.Add(48*time.Hour+30*time.Minute)) {
		t.Fatal("forgot a message the coordinator may still redeliver")
	}
	if s.acceptedLocked(a, now.Add(49*time.Hour+time.Second)) {
		t.Fatal("kept past its expiry and the slack")
	}
	for i := 0; i < seenCap+5; i++ {
		s.acceptLocked(ulid.New(), time.Time{}, now)
	}
	if len(s.rec.Accepted) != seenCap {
		t.Fatalf("%d accepted ids", len(s.rec.Accepted))
	}
}
