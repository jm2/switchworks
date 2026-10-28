// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/adapter/codexad"
	"github.com/jm2/switchworks/internal/codex/codextest"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
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
	n.continueInterrupted(s, h, time.Now(), "Switchworks restarted")
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
	n.continueInterrupted(s, h, time.Now(), "Switchworks restarted")
	if s.queueLen() != 0 || s.savedTurn() != "" {
		t.Fatalf("turned off: queue %d, saved turn %q", s.queueLen(), s.savedTurn())
	}

	h2 := &queueHandle{status: model.StatusActive}
	n2, s2 := queueNode(t, h2)
	s2.mu.Lock()
	s2.rec.ActiveTurn = "turn-1"
	s2.mu.Unlock()
	n2.continueInterrupted(s2, h2, time.Now(), "Switchworks restarted")
	if s2.queueLen() != 0 {
		t.Fatal("a message for a turn the harness still runs")
	}
}

// A turn cut off by the fake Codex app-server crashing mid-turn is
// continued once the adapter has restarted it and resumed the thread, and
// only once.
func TestContinueAfterCodexCrash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := codexad.New(codexad.Config{Bin: os.Args[0], Args: []string{"app-server"}, Env: codextest.Env(t.TempDir()), Logf: t.Logf})
	n := &Node{dir: dir, logf: t.Logf, adapters: map[string]adapter.Adapter{"codex": a}, sessions: map[string]*session{},
		closing: make(chan struct{})}
	t.Cleanup(func() {
		close(n.closing)
		_ = a.Close()
		for _, s := range n.sessionList() {
			s.mu.Lock()
			s.rec.Stopped = true
			if s.spool != nil {
				_ = s.spool.Close()
				s.spool = nil
			}
			s.mu.Unlock()
		}
		n.stateMu.Lock()
	})
	id := ulid.New()
	if _, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: id, Path: "devbox/a"},
		Spec: model.SpawnSpec{Harness: "codex", Cwd: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	s, err := n.session(id)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "idle", func() bool { h, err := s.handle(); return err == nil && h.Status() == model.StatusIdle })
	send(t, n, s, wire.MessageCmd{From: "user:web", Owner: true, Text: "CRASH now", Mode: model.ModeAuto})
	continued := func() int {
		k := 0
		for _, e := range s.events(model.KindItemCompleted) {
			if strings.Contains(e, "was cut off when Codex restarted at ") && strings.Contains(e, "echo: ") {
				k++
			}
		}
		return k
	}
	eventually(t, "the turn continued after the crash", func() bool { return continued() == 1 })
	if s.savedTurn() != "" {
		eventually(t, "the continuing turn to end", func() bool { return s.savedTurn() == "" })
	}
	time.Sleep(100 * time.Millisecond)
	if k := continued(); k != 1 || len(s.events(model.KindMessageQueued)) != 2 {
		t.Fatalf("continued %d times; message.queued %v", k, s.events(model.KindMessageQueued))
	}
}

// A turn its owner interrupted is not continued when the harness then exits
// and is restarted.
func TestNoContinueAfterOwnerInterrupt(t *testing.T) {
	h := &queueHandle{status: model.StatusActive}
	n, s := queueNode(t, h)
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "turn-1"}})
	if res := n.handleCmd(wire.Cmd{Op: wire.OpInterrupt, Session: s.rec.ID}); !res.OK {
		t.Fatalf("interrupt: %s", res.Error)
	}
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "turn-1", Status: "failed", HarnessExited: true}})
	h.setStatus(model.StatusIdle)
	h.sink(adapter.Event{Kind: model.KindSessionRegistered, Payload: model.SessionRegistered{NativeRef: h.Ref(), Resumed: true}})
	time.Sleep(50 * time.Millisecond)
	if len(h.deliveries()) != 0 || s.queueLen() != 0 {
		t.Fatal("a turn its owner interrupted was continued")
	}

	// Without the interrupt it is continued, with the harness's name.
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "turn-2"}})
	h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: "turn-2", Status: "failed", HarnessExited: true}})
	h.sink(adapter.Event{Kind: model.KindSessionRegistered, Payload: model.SessionRegistered{NativeRef: h.Ref(), Resumed: true}})
	eventually(t, "the continuing turn", func() bool { return len(h.deliveries()) == 1 })
	if text := h.deliveries()[0].in.Text; !strings.Contains(text, "(id turn-2) was cut off when fake restarted at ") {
		t.Fatalf("turn text:\n%s", text)
	}
}
