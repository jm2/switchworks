// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// A quiet message is steered into a running turn as usual.
func TestQuietSteersRunningTurn(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, steer: true}
	n, s := queueNode(t, h)
	if dr := send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "ok", Mode: model.ModeAuto, Quiet: true}); dr.Mode != model.ModeSteer {
		t.Fatalf("quiet auto into a running turn: %s", dr.Mode)
	}
	if d := h.deliveries(); len(d) != 1 || d[0].mode != adapter.DeliverSteer {
		t.Fatalf("deliveries %+v", d)
	}
}

// A quiet message to an idle session waits: no turn starts for it, the
// session settles, the sweep leaves it alone, and it goes in with the next
// message that does start a turn.
func TestQuietWaitsForNextTurn(t *testing.T) {
	defer func(s, a time.Duration) { sweepEvery, stallAfter = s, a }(sweepEvery, stallAfter)
	stallAfter = 0
	h := &queueHandle{status: model.StatusIdle, steer: true}
	n, s := queueNode(t, h)
	s.mu.Lock()
	s.busy = true // a turn has run: the session settles once nothing is left
	s.mu.Unlock()
	quiet := ulid.New()
	if dr := send(t, n, s, wire.MessageCmd{ID: quiet, From: "devbox/b", Text: "host free", Mode: model.ModeAuto, Quiet: true}); dr.Mode != model.ModeQueue {
		t.Fatalf("quiet to an idle session: %s", dr.Mode)
	}
	eventually(t, "session.settled", func() bool { return s.kindIndex(model.KindSessionSettled) >= 0 })
	n.sweepQueues(time.Now())
	n.sweepQueues(time.Now())
	n.tryDeliver(s)
	time.Sleep(20 * time.Millisecond)
	if d := h.deliveries(); len(d) != 0 || s.queueLen() != 1 {
		t.Fatalf("a quiet message started a turn: %d deliveries, %d queued", len(d), s.queueLen())
	}
	if qr := queueEntries(t, n, s); len(qr) != 1 || !qr[0].Quiet {
		t.Fatalf("queue %+v", qr)
	}

	loud := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: loud, From: "devbox/c", Text: "please look", Mode: model.ModeAuto})
	eventually(t, "one turn with both", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
	text := h.deliveries()[0].in.Text
	if !strings.Contains(text, "host free") || !strings.Contains(text, "please look") {
		t.Fatalf("turn text:\n%s", text)
	}
}

// A quiet message still expires in the queue.
func TestQuietExpires(t *testing.T) {
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	send(t, n, s, wire.MessageCmd{From: "devbox/b", Text: "fyi", Quiet: true, ExpiresAt: time.Now().Add(50 * time.Millisecond)})
	time.Sleep(80 * time.Millisecond)
	n.sweepQueues(time.Now())
	if s.queueLen() != 0 || len(s.events(model.KindMessageExpired)) != 1 || len(h.deliveries()) != 0 {
		t.Fatalf("queue %d, expired %v, deliveries %d", s.queueLen(), s.events(model.KindMessageExpired), len(h.deliveries()))
	}
}

// A person's promote makes a quiet message go now, starting a turn.
func TestQuietPromoted(t *testing.T) {
	h := &queueHandle{status: model.StatusIdle}
	n, s := queueNode(t, h)
	id := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: id, From: "devbox/b", Text: "fyi", Quiet: true})
	time.Sleep(20 * time.Millisecond)
	if len(h.deliveries()) != 0 {
		t.Fatal("a quiet message started a turn")
	}
	res := n.handleCmd(wire.Cmd{Op: wire.OpPromote, Session: s.rec.ID, Queued: &wire.QueuedCmd{MessageID: id, Mode: model.ModeSteer}})
	if !res.OK {
		t.Fatalf("promote: %s", res.Error)
	}
	eventually(t, "the promoted message's turn", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
}

// The message that starts a turn is in its batch even when quiet ones ahead
// of it fill the batch's bounds.
func TestQuietBatchHoldsLead(t *testing.T) {
	s := &session{}
	for range maxBatchMessages + 3 {
		s.rec.Queue = append(s.rec.Queue, queuedMsg{ID: ulid.New(), Text: "ack", Quiet: true})
	}
	if b := s.nextBatchLocked(); b != nil {
		t.Fatalf("a batch of quiet messages only: %d", len(b))
	}
	lead := queuedMsg{ID: ulid.New(), Text: "work"}
	s.rec.Queue = append(s.rec.Queue, lead)
	b := s.nextBatchLocked()
	if len(b) != maxBatchMessages || b[0].ID != lead.ID {
		t.Fatalf("batch of %d, first %s", len(b), b[0].ID)
	}
}

func queueEntries(t *testing.T, n *Node, s *session) []wire.QueueEntry {
	t.Helper()
	res := n.handleCmd(wire.Cmd{Op: wire.OpQueue, Session: s.rec.ID})
	var qr wire.QueueResult
	if !res.OK || json.Unmarshal(res.Data, &qr) != nil {
		t.Fatalf("queue: %+v", res)
	}
	return qr.Messages
}

// A quiet message's header says so, and that it needs no answer.
func TestQuietHeader(t *testing.T) {
	text := formatMessage(queuedMsg{ID: "m1", From: "devbox/b", Text: "host free", Quiet: true})
	if !strings.HasPrefix(text, "[switchworks msg=m1 from=devbox/b kind=notify quiet=1]\n(Quiet: ") || !strings.HasSuffix(text, "\nhost free") {
		t.Fatalf("quiet message:\n%s", text)
	}
	if strings.Contains(formatMessage(queuedMsg{ID: "m2", From: "devbox/b", Text: "x"}), "quiet") {
		t.Fatal("a plain message says quiet")
	}
}
