// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// queueAdapter starts one queueHandle.
type queueAdapter struct{ h *queueHandle }

func (a *queueAdapter) Name() string { return "fake" }
func (a *queueAdapter) Start(_ context.Context, _ adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	a.h.sink = sink
	sink(adapter.Event{Kind: model.KindSessionRegistered, Payload: model.SessionRegistered{NativeRef: a.h.Ref(), Caps: a.h.Caps()}})
	sink(adapter.Event{Kind: model.KindSessionStatus, Payload: model.SessionStatus{Status: a.h.Status()}})
	return a.h, nil
}
func (a *queueAdapter) Resume(context.Context, model.NativeRef, adapter.StartSpec, adapter.Sink) (adapter.Handle, error) {
	return nil, os.ErrNotExist
}
func (a *queueAdapter) Close() error { return nil }

// queueHandle records what it is given. busy is how many deliveries to
// refuse with ErrBusy (while still reporting idle), status what it reports,
// and steer whether it can steer.
type queueHandle struct {
	sink adapter.Sink

	mu     sync.Mutex
	status string
	busy   int
	steer  bool
	gate   chan struct{} // when set, Deliver waits on it
	got    []delivery
}

type delivery struct {
	in   adapter.Input
	mode adapter.DeliveryMode
}

func (h *queueHandle) Ref() model.NativeRef { return model.NativeRef{Harness: "fake", ID: "t1"} }
func (h *queueHandle) Caps() model.Caps {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.Caps{Message: true, Steer: h.steer, Interrupt: true}
}
func (h *queueHandle) Status() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}
func (h *queueHandle) Deliver(_ context.Context, in adapter.Input, mode adapter.DeliveryMode) error {
	h.mu.Lock()
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if mode == adapter.DeliverSteer && h.status != model.StatusActive {
		return adapter.ErrNotActive
	}
	if mode == adapter.DeliverQueue && h.busy > 0 {
		h.busy--
		return adapter.ErrBusy
	}
	h.got = append(h.got, delivery{in, mode})
	return nil
}
func (h *queueHandle) Interrupt(context.Context) error               { return nil }
func (h *queueHandle) Resolve(context.Context, string, string) error { return nil }
func (h *queueHandle) Stop(context.Context) error                    { return nil }

func (h *queueHandle) deliveries() []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]delivery(nil), h.got...)
}

// setStatus changes the status and reports it, as a harness does.
func (h *queueHandle) setStatus(st string) {
	h.mu.Lock()
	h.status = st
	h.mu.Unlock()
	h.sink(adapter.Event{Kind: model.KindSessionStatus, Payload: model.SessionStatus{Status: st}})
}

// queueNode starts a node with one session on h.
func queueNode(t *testing.T, h *queueHandle) (*Node, *session) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{dir: dir, logf: t.Logf, adapters: map[string]adapter.Adapter{"fake": &queueAdapter{h: h}}, sessions: map[string]*session{},
		closing: make(chan struct{})}
	t.Cleanup(func() {
		close(n.closing)
		for _, s := range n.sessionList() {
			s.mu.Lock()
			s.rec.Stopped = true // no late retries
			if s.spool != nil {
				_ = s.spool.Close()
				s.spool = nil
			}
			s.mu.Unlock()
		}
	})
	id := ulid.New()
	if _, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: id, Path: "devbox/a"},
		Spec: model.SpawnSpec{Harness: "fake", Cwd: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	s, err := n.session(id)
	if err != nil {
		t.Fatal(err)
	}
	return n, s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *session) queueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rec.Queue)
}

func (s *session) eventKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ev := range s.unacked {
		out = append(out, ev.Kind)
	}
	return out
}

func send(t *testing.T, n *Node, s *session, m wire.MessageCmd) wire.DeliverResult {
	t.Helper()
	if m.ID == "" {
		m.ID = ulid.New()
	}
	res := n.handleCmd(wire.Cmd{Op: wire.OpDeliver, Session: s.rec.ID, Message: &m})
	if !res.OK {
		t.Fatalf("deliver: %s", res.Error)
	}
	var dr wire.DeliverResult
	_ = json.Unmarshal(res.Data, &dr)
	return dr
}

// #39: a harness that answers ErrBusy while the session is idle (and stays
// idle, so no status event follows) does not stall the queue: the delivery
// is retried and the message delivered.
func TestQueueBusyWhileIdleRetries(t *testing.T) {
	defer func(b []time.Duration) { busyBackoff = b }(busyBackoff)
	busyBackoff = []time.Duration{20 * time.Millisecond}
	h := &queueHandle{status: model.StatusIdle, busy: 1}
	n, s := queueNode(t, h)
	statuses := func() int {
		c := 0
		for _, k := range s.eventKinds() {
			if k == model.KindSessionStatus {
				c++
			}
		}
		return c
	}
	before := statuses()
	send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "hello"})
	eventually(t, "the message delivered after ErrBusy", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
	if got := statuses(); got != before {
		t.Fatalf("%d new status events; the retry must not need one", got-before)
	}
}

// The periodic sweep starts delivery for a session left idle with a queue.
func TestSweepKicksStalledQueue(t *testing.T) {
	defer func(d time.Duration) { stallAfter = d }(stallAfter)
	stallAfter = 0
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	// A queue left behind with nothing to trigger it (as after a stall).
	s.mu.Lock()
	s.rec.Queue = append(s.rec.Queue, queuedMsg{ID: ulid.New(), From: "devbox/b", Text: "stuck"})
	s.mu.Unlock()
	now := time.Now()
	n.sweepQueues(now) // first sight
	if len(h.deliveries()) != 0 {
		t.Fatal("delivered on first sight")
	}
	n.sweepQueues(now.Add(time.Second))
	eventually(t, "the sweep to deliver", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
}

// events returns the payloads of the session's events of one kind.
func (s *session) events(kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ev := range s.unacked {
		if ev.Kind == kind {
			out = append(out, string(ev.Payload))
		}
	}
	return out
}

// #32: messages queued while a session works go in as one turn when it is
// idle, each with its own header and in order, the owner's first; each is
// reported delivered, and the queue's length is reported as it changes.
func TestQueueCoalescesOwnerFirst(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	ids := []string{ulid.New(), ulid.New(), ulid.New(), ulid.New()}
	send(t, n, s, wire.MessageCmd{ID: ids[0], From: "devbox/b", Text: "one"})
	send(t, n, s, wire.MessageCmd{ID: ids[1], From: "devbox/c", Text: "two", Kind: model.MsgAsk})
	send(t, n, s, wire.MessageCmd{ID: ids[2], From: "user:web", Via: "admin", Owner: true, Text: "owner here"})
	send(t, n, s, wire.MessageCmd{ID: ids[3], From: "user:cli", Via: "token:laptop", Text: "a person"})
	s.mu.Lock()
	var order []string
	for _, q := range s.rec.Queue {
		order = append(order, q.ID)
	}
	s.mu.Unlock()
	if want := []string{ids[2], ids[3], ids[0], ids[1]}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("queue order %v, want owner, person, then agents in order %v", order, want)
	}
	if dup := send(t, n, s, wire.MessageCmd{ID: ids[0], From: "devbox/b", Text: "one"}); !dup.Duplicate {
		t.Fatal("redelivery not deduplicated")
	}

	h.setStatus(model.StatusIdle)
	eventually(t, "one coalesced turn", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
	time.Sleep(20 * time.Millisecond) // no second turn follows
	got := h.deliveries()
	if len(got) != 1 {
		t.Fatalf("%d turns, want 1", len(got))
	}
	text := got[0].in.Text
	if !strings.HasPrefix(text, "owner here\n\n") {
		t.Fatalf("the owner's plain message must come first:\n%s", text)
	}
	at := func(sub string) int {
		i := strings.Index(text, sub)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", sub, text)
		}
		return i
	}
	if !(at("from=user:cli via=token:laptop kind=notify]\na person") < at("msg="+ids[0]+" from=devbox/b kind=notify]\none") &&
		at("msg="+ids[0]) < at("msg="+ids[1]+" from=devbox/c kind=ask]")) {
		t.Fatalf("headers out of order:\n%s", text)
	}
	if got[0].in.ClientMessageID != ids[2] {
		t.Fatalf("client message id %s", got[0].in.ClientMessageID)
	}
	delivered := strings.Join(s.events(model.KindMessageDelivered), "\n")
	for _, id := range ids {
		if strings.Count(delivered, id) != 1 {
			t.Fatalf("message %s not reported delivered once:\n%s", id, delivered)
		}
	}
	if q := s.events(model.KindSessionQueue); len(q) != 5 || q[3] != `{"queued":4}` || q[4] != `{"queued":0}` {
		t.Fatalf("queue reports %v", q)
	}
}

// A batch stays within its bounds; the rest goes in the next turn.
func TestBatchLen(t *testing.T) {
	var q []queuedMsg
	for i := 0; i < maxBatchMessages+3; i++ {
		q = append(q, queuedMsg{Text: "x"})
	}
	if n := batchLen(q); n != maxBatchMessages {
		t.Fatalf("batch of %d", n)
	}
	big := strings.Repeat("y", maxBatchText)
	if n := batchLen([]queuedMsg{{Text: big}, {Text: big}}); n != 1 {
		t.Fatalf("text bound: batch of %d", n)
	}
	img := func(k int) []model.Attachment { return make([]model.Attachment, k) }
	if n := batchLen([]queuedMsg{{Attachments: img(3)}, {Attachments: img(2)}, {Text: "z"}}); n != 1 {
		t.Fatalf("image bound: batch of %d", n)
	}
}

// Interrupts go ahead of their own rank, never ahead of the owner's.
func TestEnqueueRanks(t *testing.T) {
	var q []queuedMsg
	q = enqueue(q, queuedMsg{ID: "a1", From: "h/a"}, false)
	q = enqueue(q, queuedMsg{ID: "o1", From: "user:web", Owner: true}, false)
	q = enqueue(q, queuedMsg{ID: "a2", From: "h/b"}, true)
	q = enqueue(q, queuedMsg{ID: "p1", From: "user:cli"}, false)
	q = enqueue(q, queuedMsg{ID: "o2", From: "user:web", Owner: true, Kind: model.MsgAsk}, false) // has a header
	var got []string
	for _, m := range q {
		got = append(got, m.ID)
	}
	if strings.Join(got, ",") != "o1,p1,o2,a2,a1" {
		t.Fatalf("order %v", got)
	}
}

// #40: auto goes into a running turn on a harness that can steer, and
// queues when nothing runs or the harness cannot steer.
func TestAutoSteersRunningTurn(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, steer: true}
	n, s := queueNode(t, h)
	if dr := send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "mid-turn", Mode: model.ModeAuto}); dr.Mode != model.ModeSteer {
		t.Fatalf("auto into a running turn: %s", dr.Mode)
	}
	if got := h.deliveries(); len(got) != 1 || got[0].mode != adapter.DeliverSteer {
		t.Fatalf("deliveries %+v", got)
	}
	h.setStatus(model.StatusIdle)
	if dr := send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "idle", Mode: model.ModeAuto}); dr.Mode != model.ModeQueue {
		t.Fatalf("auto to an idle session: %s", dr.Mode)
	}
	eventually(t, "a new turn", func() bool { d := h.deliveries(); return len(d) == 2 && d[1].mode == adapter.DeliverQueue })

	h2 := &queueHandle{status: model.StatusActive} // cannot steer
	n2, s2 := queueNode(t, h2)
	if dr := send(t, n2, s2, wire.MessageCmd{From: "devbox/b", Text: "x", Mode: model.ModeAuto}); dr.Mode != model.ModeQueue {
		t.Fatalf("auto on a harness that cannot steer: %s", dr.Mode)
	}
	if len(h2.deliveries()) != 0 || s2.queueLen() != 1 {
		t.Fatal("not queued")
	}
}

// #40: messages that arrive while a steer is in flight go in together as
// one steer input.
func TestSteersBatchedWhileInFlight(t *testing.T) {
	gate := make(chan struct{})
	h := &queueHandle{status: model.StatusActive, steer: true, gate: gate}
	n, s := queueNode(t, h)
	results := make(chan wire.DeliverResult, 10)
	go func() {
		results <- send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "first", Mode: model.ModeAuto})
	}()
	eventually(t, "the first steer in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.steering && len(s.steerBuf) == 0
	})
	for i := 0; i < 9; i++ {
		go func() {
			results <- send(t, n, s, wire.MessageCmd{From: "devbox/c", Text: "later", Mode: model.ModeAuto})
		}()
	}
	eventually(t, "nine steers waiting", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.steerBuf) == 9
	})
	close(gate)
	for i := 0; i < 10; i++ {
		if dr := <-results; dr.Mode != model.ModeSteer {
			t.Fatalf("result %d: %s", i, dr.Mode)
		}
	}
	got := h.deliveries()
	if len(got) != 2 || strings.Count(got[1].in.Text, "from=devbox/c kind=notify]\nlater") != 9 {
		t.Fatalf("%d steers; want the first, then the other nine as one: %+v", len(got), got)
	}
	if delivered := s.events(model.KindMessageDelivered); len(delivered) != 10 {
		t.Fatalf("%d delivered events", len(delivered))
	}
}
