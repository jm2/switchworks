// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/wire"
)

func (s *session) savedTurn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.ActiveTurn
}

// savedState reads the session's record back from state.json.
func savedState(t *testing.T, n *Node, id string) record {
	t.Helper()
	recs, err := n.loadState()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("session %s not in state.json", id)
	return record{}
}

// The running turn is saved as it starts and cleared as it completes; one
// that completes while the node shuts down stays saved.
func TestActiveTurnSaved(t *testing.T) {
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "turn-1"}})
	if got := savedState(t, n, s.rec.ID).ActiveTurn; got != "turn-1" {
		t.Fatalf("saved active turn %q", got)
	}
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "turn-1", Status: "completed"}})
	if got := savedState(t, n, s.rec.ID).ActiveTurn; got != "" {
		t.Fatalf("a completed turn is still saved: %q", got)
	}

	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "turn-2"}})
	open, shut := n.closing, make(chan struct{}) // the node shutting down
	close(shut)
	n.closing = shut
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "turn-2", Status: "interrupted"}})
	n.closing = open
	if got := s.savedTurn(); got != "turn-2" {
		t.Fatalf("a turn cut off by the shutdown is not kept: %q", got)
	}
}

// A person's interrupt clears the saved turn: a restart does not continue
// a turn its owner stopped.
func TestInterruptClearsActiveTurn(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "turn-1"}})
	if res := n.handleCmd(wire.Cmd{Op: wire.OpInterrupt, Session: s.rec.ID}); !res.OK {
		t.Fatalf("interrupt: %s", res.Error)
	}
	if got := savedState(t, n, s.rec.ID).ActiveTurn; got != "" {
		t.Fatalf("an interrupted turn is still saved: %q", got)
	}
}

// A session resumed after the node went down mid-turn gets one system
// message, which starts a new turn.
func TestContinueInterruptedTurn(t *testing.T) {
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	n.adapters["fake"].(*queueAdapter).resumable = true
	s.mu.Lock()
	s.rec.ActiveTurn, s.h = "turn-7", nil
	s.mu.Unlock()
	n.resume(s)
	eventually(t, "a turn continuing it", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
	text := h.deliveries()[0].in.Text
	if !strings.Contains(text, "from="+model.SystemSender+" kind=system]") ||
		!strings.Contains(text, "Your previous turn (id turn-7) was cut off when Switchworks restarted at ") ||
		!strings.Contains(text, "Continue where you left off; check the state of anything you were running.") {
		t.Fatalf("turn text:\n%s", text)
	}
	if got := savedState(t, n, s.rec.ID).ActiveTurn; got != "" {
		t.Fatalf("the cut-off turn is still saved: %q", got)
	}
	// Once per interruption.
	n.continueInterrupted(s, h, time.Now())
	time.Sleep(20 * time.Millisecond)
	if len(h.deliveries()) != 1 || s.queueLen() != 0 {
		t.Fatal("a second message for the same interruption")
	}
}

// No message when it is turned off, or when the harness still runs the turn.
func TestContinueInterruptedOff(t *testing.T) {
	off := false
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	n.cfg.ContinueInterrupted = &off
	s.mu.Lock()
	s.rec.ActiveTurn = "turn-1"
	s.mu.Unlock()
	n.continueInterrupted(s, h, time.Now())
	if s.queueLen() != 0 || s.savedTurn() != "" {
		t.Fatalf("turned off: queue %d, saved turn %q", s.queueLen(), s.savedTurn())
	}

	h2 := &queueHandle{status: model.StatusActive}
	n2, s2 := queueNode(t, h2)
	s2.mu.Lock()
	s2.rec.ActiveTurn = "turn-1"
	s2.mu.Unlock()
	n2.continueInterrupted(s2, h2, time.Now())
	if s2.queueLen() != 0 {
		t.Fatal("a message for a turn the harness still runs")
	}
}
