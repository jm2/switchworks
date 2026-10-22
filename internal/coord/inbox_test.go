// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/wire"
)

func compact(t *testing.T, res fleet.InboxResult) []fleet.InboxMessage {
	t.Helper()
	msgs, ok := res.Messages.([]fleet.InboxMessage)
	if !ok {
		t.Fatalf("messages are %T, want compact", res.Messages)
	}
	return msgs
}

func texts(msgs []fleet.InboxMessage) string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return strings.Join(out, ",")
}

// fleet_inbox lists unread messages only, oldest first and compact, and
// moves the session's cursor past them; a message delivered as a turn
// counts as read.
func TestInboxUnread(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result { return wire.Result{OK: true} })
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	inbox := func(args fleet.InboxArgs) fleet.InboxResult {
		t.Helper()
		res, err := r.inbox(ctx, from(b), args)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	send := func(text string) model.Message {
		t.Helper()
		msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: text})
		if err != nil || msgs[0].State != model.MsgDelivered {
			t.Fatalf("send: %+v %v", msgs, err)
		}
		return msgs[0]
	}

	// Idle, nothing new: the smallest answer.
	idle, _ := json.Marshal(inbox(fleet.InboxArgs{}))
	if string(idle) != `{"messages":[]}` {
		t.Fatalf("idle inbox: %s", idle)
	}

	for _, x := range []string{"one", "two", "three"} {
		send(x)
	}
	res := inbox(fleet.InboxArgs{Limit: 2})
	if got := texts(compact(t, res)); got != "one,two" || res.More != 1 {
		t.Fatalf("first page: %q more=%d", got, res.More)
	}
	m := compact(t, res)[0]
	if m.Kind != model.MsgNotify || m.From != a.Path || m.To != "" || m.CreatedAt.IsZero() || m.ID == "" {
		t.Fatalf("compact message: %+v", m)
	}
	if got := texts(compact(t, inbox(fleet.InboxArgs{}))); got != "three" {
		t.Fatalf("second page: %q", got)
	}
	if got := compact(t, inbox(fleet.InboxArgs{})); len(got) != 0 {
		t.Fatalf("read again: %+v", got)
	}

	// Delivered as a turn: already seen.
	four := send("four")
	r.onDelivered(b.ID, four.ID, "queue")
	if got := compact(t, inbox(fleet.InboxArgs{})); len(got) != 0 {
		t.Fatalf("message seen as a turn listed: %+v", got)
	}
	if sess, _ := s.store.Session(b.ID); sess.InboxRead != four.ID {
		t.Fatalf("cursor %q, want %q", sess.InboxRead, four.ID)
	}

	// A long text is cut on a character boundary, saying how much is left
	// out.
	long := strings.Repeat("é", 1500) // 3000 bytes
	send(long)
	got := compact(t, inbox(fleet.InboxArgs{}))
	if len(got) != 1 || !utf8.ValidString(got[0].Text) || !strings.HasSuffix(got[0].Text, "… [1000 more bytes cut]") {
		t.Fatalf("cut text: %q", got[0].Text[len(got[0].Text)-40:])
	}

	// A reply carries re; messages b sent show their recipient in all.
	ask := r.ask(ctx, from(b), fleet.AskArgs{To: "a", Text: "q?", WaitSeconds: new(int)})
	if _, err := r.reply(ctx, from(a), fleet.ReplyArgs{MessageID: ask.MessageID, Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	got = compact(t, inbox(fleet.InboxArgs{}))
	if len(got) != 1 || got[0].Kind != model.MsgReply || got[0].Re != ask.MessageID {
		t.Fatalf("reply: %+v", got)
	}

	all := compact(t, inbox(fleet.InboxArgs{All: true, Limit: 3}))
	if len(all) != 3 || all[0].Text != "answer" || all[1].Text != "q?" || all[1].To != "a" {
		t.Fatalf("all: %+v", all)
	}
	full, ok := inbox(fleet.InboxArgs{All: true, Verbose: true, Limit: 1}).Messages.([]model.Message)
	if !ok || len(full) != 1 || full[0].ToSession != b.ID || full[0].State == "" {
		t.Fatalf("verbose: %+v", full)
	}

	// A ticket check lists nothing and leaves the cursor alone.
	send("five")
	tk := inbox(fleet.InboxArgs{Ticket: ask.MessageID})
	if tk.Ticket == nil || tk.Ticket.Status != fleet.AskReplied || len(compact(t, tk)) != 0 {
		t.Fatalf("ticket: %+v", tk)
	}
	if got := texts(compact(t, inbox(fleet.InboxArgs{}))); got != "five" {
		t.Fatalf("after ticket: %q", got)
	}
}

// Messages still pending (held replies, an offline host) are not unread.
func TestInboxSkipsPending(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.inbox(ctx, from(b), fleet.InboxArgs{})
	if err != nil || len(compact(t, res)) != 0 {
		t.Fatalf("pending listed: %+v %v", res, err)
	}
	if sess, _ := s.store.Session(b.ID); sess.InboxRead != "" {
		t.Fatalf("cursor moved: %q", sess.InboxRead)
	}
}
