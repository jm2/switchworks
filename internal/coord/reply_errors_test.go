// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/wire"
)

// fleet_reply to something that is not an open ask says what it is and
// what to do instead; fleet_inbox marks the messages that can be answered.
func TestReplyErrors(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result { return wire.Result{OK: true} })
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	c := addSession(t, s, "devbox", "c")

	msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "fyi"})
	if err != nil {
		t.Fatal(err)
	}
	notify := msgs[0]
	ask := r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "q?", WaitSeconds: new(int)})

	inbox, err := r.inbox(ctx, from(b), fleet.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]bool{}
	for _, m := range compact(t, inbox) {
		marks[m.ID] = m.Replyable
	}
	if len(marks) != 2 || marks[notify.ID] || !marks[ask.MessageID] {
		t.Fatalf("replyable marks: %+v", marks)
	}

	// A notify: say so, and point at fleet_send.
	_, err = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: notify.ID, Text: "ok"})
	if err == nil || !strings.Contains(err.Error(), "is a notify") || !strings.Contains(err.Error(), "fleet_send") ||
		!strings.Contains(err.Error(), a.Path) {
		t.Fatalf("reply to a notify: %v", err)
	}
	// Someone else's notify: no more than for a missing id.
	_, err = r.reply(ctx, from(c), fleet.ReplyArgs{MessageID: notify.ID, Text: "ok"})
	if err == nil || !strings.Contains(err.Error(), "no ask with id") {
		t.Fatalf("reply to another's notify: %v", err)
	}

	// Answered twice: the second says when, by whom, and that it was us.
	first, err := r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: ask.MessageID, Text: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: ask.MessageID, Text: "yes again"})
	if !errors.Is(err, ErrAskClosed) || !strings.Contains(err.Error(), "answered at 20") ||
		!strings.Contains(err.Error(), "by "+b.Path) || !strings.Contains(err.Error(), "your own earlier reply") ||
		!strings.Contains(err.Error(), first.ReplyID) {
		t.Fatalf("second reply: %v", err)
	}

	// Once answered, the ask is no longer marked.
	all, err := r.inbox(ctx, from(b), fleet.InboxArgs{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range compact(t, all) {
		if m.Replyable {
			t.Fatalf("answered ask still replyable: %+v", m)
		}
	}
}
