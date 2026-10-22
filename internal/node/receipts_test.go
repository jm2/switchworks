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

	// Steered into a turn that is interrupted: not seen.
	turn("t2")
	c := ulid.New()
	if dr := send(t, n, s, wire.MessageCmd{ID: c, From: "user:web", Owner: true, Text: "three", Mode: model.ModeAuto}); dr.Mode != model.ModeSteer {
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
	send(t, n, s, wire.MessageCmd{ID: d, From: "user:web", Owner: true, Text: "four", Mode: model.ModeAuto})
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
