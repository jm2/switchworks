// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
