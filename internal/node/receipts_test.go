// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// #45: when the turn that carried messages completes, each is reported
// seen with that turn and its last agent message: queued ones by the turn
// their delivery started, steered ones by the turn they went into. A turn
// that does not complete leaves them unseen.
func TestSeenReceipts(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, steer: true}
	n, s := queueNode(t, h)
	turn := func(id string) {
		h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: id}})
	}
	reply := func(turnID, item string) {
		h.sink(adapter.Event{Kind: model.KindItemCompleted, Payload: model.Item{TurnID: turnID, ItemID: item, ItemType: "agentMessage", Text: "ok"}})
	}
	done := func(id, status string) {
		h.sink(adapter.Event{Kind: model.KindTurnCompleted, Payload: model.TurnCompleted{TurnID: id, Status: status}})
	}
	seen := func() []model.MessageSeen {
		var out []model.MessageSeen
		for _, p := range s.events(model.KindMessageSeen) {
			var m model.MessageSeen
			_ = json.Unmarshal([]byte(p), &m)
			out = append(out, m)
		}
		return out
	}

	// Queued, then delivered as one turn.
	a, b := ulid.New(), ulid.New()
	send(t, n, s, wire.MessageCmd{ID: a, From: "user:web", Owner: true, Text: "one", Mode: model.ModeQueue})
	send(t, n, s, wire.MessageCmd{ID: b, From: "devbox/b", Text: "two", Mode: model.ModeQueue})
	h.setStatus(model.StatusIdle)
	eventually(t, "the queued turn", func() bool { return len(h.deliveries()) == 1 && s.queueLen() == 0 })
	h.setStatus(model.StatusActive)
	turn("t1")
	reply("t1", "i1")
	reply("t1", "i2")
	if len(seen()) != 0 {
		t.Fatal("seen before the turn completed")
	}
	done("t1", "completed")
	if got := seen(); len(got) != 2 || got[0].MessageID != a || got[1].MessageID != b || got[0].TurnID != "t1" || got[1].ItemID != "i2" {
		t.Fatalf("seen %+v", got)
	}

	// Steered into a turn that is interrupted: not seen. (An agent's: a
	// person's waits for the turn's next step, TestPersonSteerAtStep.)
	turn("t2")
	c := ulid.New()
	if dr := send(t, n, s, wire.MessageCmd{ID: c, From: "devbox/b", Text: "three", Mode: model.ModeAuto}); dr.Mode != model.ModeSteer {
		t.Fatalf("mode %s", dr.Mode)
	}
	done("t2", "interrupted")
	if got := seen(); len(got) != 2 {
		t.Fatalf("seen after an interrupted turn %+v", got)
	}

	// Steered into a turn that completes: seen, and reported delivered
	// before that.
	turn("t3")
	d := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: d, From: "devbox/b", Text: "four", Mode: model.ModeAuto})
	reply("t3", "i3")
	done("t3", "completed")
	if got := seen(); len(got) != 3 || got[2].MessageID != d || got[2].TurnID != "t3" || got[2].ItemID != "i3" {
		t.Fatalf("seen %+v", got)
	}
	kinds := strings.Join(s.eventKinds(), ",")
	if strings.LastIndex(kinds, model.KindMessageDelivered) > strings.LastIndex(kinds, model.KindMessageSeen) {
		t.Fatalf("delivered after seen: %s", kinds)
	}
}

// A person's steer into a running turn waits in the queue for the turn's
// next step, when the agent would read it anyway: until then it can be
// cancelled; at the step it goes into the turn.
func TestPersonSteerAtStep(t *testing.T) {
	h := &queueHandle{status: model.StatusActive, steer: true}
	n, s := queueNode(t, h)
	h.sink(adapter.Event{Kind: model.KindTurnStarted, Payload: model.TurnStarted{TurnID: "t1"}})
	step := func() {
		h.sink(adapter.Event{Kind: model.KindItemCompleted, Payload: model.Item{TurnID: "t1", ItemID: ulid.New(), ItemType: "commandExecution"}})
	}

	a := ulid.New()
	if dr := send(t, n, s, wire.MessageCmd{ID: a, From: "user:web", Owner: true, Text: "stop that", Mode: model.ModeAuto}); dr.Mode != model.ModeQueue {
		t.Fatalf("a person's steer: %s, want queue (held)", dr.Mode)
	}
	if len(h.deliveries()) != 0 || s.queueLen() != 1 {
		t.Fatal("steered at once")
	}
	if err := n.cancelQueued(s, &wire.QueuedCmd{MessageID: a}); err != nil {
		t.Fatalf("cancel before the step: %v", err)
	}
	step()
	if len(h.deliveries()) != 0 {
		t.Fatal("a cancelled steer went in")
	}

	b := ulid.New()
	send(t, n, s, wire.MessageCmd{ID: b, From: "user:web", Owner: true, Text: "and this", Mode: model.ModeAuto})
	step()
	eventually(t, "the held steer in at the step", func() bool {
		d := h.deliveries()
		return len(d) == 1 && d[0].mode == adapter.DeliverSteer && s.queueLen() == 0
	})
}
