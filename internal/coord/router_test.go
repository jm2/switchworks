// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/store"
)

// testServer is a coordinator with a store and no nodes: every host is
// offline, so session deliveries stay pending, which lets the tests look at
// routing decisions on their own.
func testServer(t *testing.T, g Guards) *Server {
	t.Helper()
	st, err := store.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, logf: t.Logf, nodes: map[string]*nodeConn{},
		subs: map[string]map[chan model.Event]struct{}{}, fleet: map[chan FleetEvent]struct{}{}, acts: map[string]*activity{}}
	s.router = newRouter(s, g)
	t.Cleanup(func() {
		s.router.close()
		_ = st.Close()
	})
	return s
}

func addSession(t *testing.T, s *Server, host, name string, mut ...func(*model.Session)) model.Session {
	t.Helper()
	now := time.Now().UTC()
	sess := model.Session{ID: "s-" + host + "-" + name, Path: host + "/" + name, Host: host, Name: name,
		Harness: "codex", Status: model.StatusIdle, CreatedAt: now, UpdatedAt: now}
	for _, m := range mut {
		m(&sess)
	}
	if err := s.store.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func from(sess model.Session) sender { return sender{sess: &sess} }

func TestResolve(t *testing.T) {
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.Labels = map[string]string{"role": "lead"} })
	b := addSession(t, s, "workbox", "helper", func(x *model.Session) {
		x.Labels = map[string]string{"role": "helper"}
		x.Parent = a.ID
	})
	addSession(t, s, "devbox", "helper", func(x *model.Session) { x.Labels = map[string]string{"role": "helper"} })
	addSession(t, s, "workbox", "old", func(x *model.Session) { x.Status = model.StatusStopped })

	cases := []struct {
		from  sender
		to    string
		bcast bool
		want  []string
		err   string
	}{
		{from(a), "workbox/helper", false, []string{"workbox/helper"}, ""},
		{from(a), b.ID, false, []string{"workbox/helper"}, ""},
		{from(a), "lead", false, []string{"devbox/lead"}, ""},
		{from(a), "helper", false, nil, "ambiguous"},
		{from(a), "old", false, nil, "no live session"},
		{from(a), "nobody/x", false, nil, "no session"},
		{from(b), "parent", false, []string{"devbox/lead"}, ""},
		{from(a), "parent", false, nil, "no parent"},
		{from(a), "@role=helper", false, []string{"devbox/helper"}, ""},
		{from(a), "@role=helper", true, []string{"devbox/helper", "workbox/helper"}, ""},
		{from(a), "@role=helper,host=workbox", false, []string{"workbox/helper"}, ""},
		{from(a), "@harness=codex", true, []string{"devbox/helper", "workbox/helper"}, ""}, // not itself, not stopped
		{from(a), "@role=nobody", false, nil, "no live session matches"},
		{from(a), "@role", false, nil, "bad selector"},
		{from(a), "user:jm", false, []string{"user:jm"}, ""},
		{from(a), "user:bad name", false, nil, "user"},
		{from(a), "", false, nil, "missing recipient"},
	}
	for _, c := range cases {
		ts, err := r.resolve(c.from, c.to, c.bcast)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("resolve(%q): err %v, want %q", c.to, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolve(%q): %v", c.to, err)
			continue
		}
		var got []string
		for _, x := range ts {
			if x.sess != nil {
				got = append(got, x.sess.Path)
			} else {
				got = append(got, x.user)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("resolve(%q, broadcast=%v) = %v, want %v", c.to, c.bcast, got, c.want)
		}
	}
}

func TestSendGuards(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{SendPerMinute: 2, AskPerMinute: 1, MaxHops: 3})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")

	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/a", Text: "x"}); err == nil {
		t.Fatal("self-send accepted")
	}
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "  "}); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: strings.Repeat("x", fleet.MaxTextBytes+1)}); err == nil {
		t.Fatal("oversized text accepted")
	}
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x", Mode: "shout"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := r.send(ctx, from(a), model.MsgAsk, fleet.SendArgs{To: "@harness=codex", Text: "x", Broadcast: true}); err == nil {
		t.Fatal("broadcast ask accepted")
	}

	// Rate limit: two sends a minute per pair; asks are counted apart.
	for k := 0; k < 2; k++ {
		msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x"})
		if err != nil || len(msgs) != 1 || msgs[0].State != model.MsgPending || msgs[0].ToSession != b.ID {
			t.Fatalf("send %d: %+v %v", k, msgs, err)
		}
	}
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x"}); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("third send: %v", err)
	}
	if res := r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "q", WaitSeconds: new(int)}); res.Status != fleet.AskPending {
		t.Fatalf("ask: %+v", res)
	}
	if res := r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "q", WaitSeconds: new(int)}); res.Status != fleet.AskFailed || !strings.Contains(res.Error, "rate limit") {
		t.Fatalf("second ask: %+v", res)
	}
	// People are not rate limited.
	for k := 0; k < 5; k++ {
		if _, err := r.send(ctx, sender{user: "user:jm"}, model.MsgNotify, fleet.SendArgs{To: "b", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	// Hops: b is working on a message that is already 3 hops deep.
	r.inbound[b.ID] = trigger{hops: 3}
	if _, err := r.send(ctx, from(b), model.MsgNotify, fleet.SendArgs{To: "a", Text: "x"}); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("hop limit: %v", err)
	}
	r.inbound[b.ID] = trigger{hops: 2}
	msgs, err := r.send(ctx, from(b), model.MsgNotify, fleet.SendArgs{To: "a", Text: "x"})
	if err != nil || msgs[0].Hops != 3 {
		t.Fatalf("hop 3: %+v %v", msgs, err)
	}
}

func TestAskReply(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "workbox", "b")
	c := addSession(t, s, "workbox", "c")

	// A waiting asker gets the answer inline.
	var wg sync.WaitGroup
	var res fleet.AskResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := 10
		res = r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "6x7?", WaitSeconds: &w})
	}()
	var ask model.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ms := s.store.Messages(store.MessageFilter{Session: b.ID}); len(ms) == 1 {
			r.mu.Lock()
			n := len(r.waiters[ms[0].ID])
			r.mu.Unlock()
			if n == 1 {
				ask = ms[0]
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ask.ID == "" {
		t.Fatal("ask never registered a waiter")
	}
	if _, err := r.reply(ctx, from(c), fleet.ReplyArgs{MessageID: ask.ID, Text: "no"}); err == nil {
		t.Fatal("a session that was not asked replied")
	}
	rr, err := r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: ask.ID, Text: "42"})
	if err != nil || !rr.Inline || rr.State != model.MsgDelivered {
		t.Fatalf("reply: %+v %v", rr, err)
	}
	wg.Wait()
	if res.Status != fleet.AskReplied || res.Reply != "42" || res.ReplyFrom != "workbox/b" {
		t.Fatalf("ask result: %+v", res)
	}
	if _, err := r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: ask.ID, Text: "43"}); err == nil {
		t.Fatal("answered twice")
	}
	if m, _ := s.store.Message(ask.ID); m.State != model.MsgAnswered || m.ReplyID != rr.ReplyID {
		t.Fatalf("ask record: %+v", m)
	}

	// No waiter: the reply goes to the asker as a message.
	res = r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "later?", WaitSeconds: new(int)})
	rr, err = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: res.MessageID, Text: "now"})
	if err != nil || rr.Inline || rr.State != model.MsgPending {
		t.Fatalf("queued reply: %+v %v", rr, err)
	}
	if rep, _ := s.store.Message(rr.ReplyID); rep.ToSession != a.ID || rep.ReplyTo != res.MessageID || rep.Kind != model.MsgReply {
		t.Fatalf("reply record: %+v", rep)
	}
	// The ticket reports it.
	ib, err := r.inbox(ctx, from(a), fleet.InboxArgs{Ticket: res.MessageID})
	if err != nil || ib.Ticket.Status != fleet.AskReplied || ib.Ticket.Reply != "now" {
		t.Fatalf("ticket: %+v %v", ib.Ticket, err)
	}
	if _, err := r.inbox(ctx, from(c), fleet.InboxArgs{Ticket: res.MessageID}); err == nil {
		t.Fatal("another session read the ticket")
	}

	// Held: the asker said it will collect the reply itself, so the reply
	// is kept back rather than queued as a turn, then collected.
	res = r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "held?", WaitSeconds: new(int), HoldSeconds: 60})
	rr, err = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: res.MessageID, Text: "kept"})
	if err != nil || rr.State != fleet.ReplyHeld {
		t.Fatalf("held reply: %+v %v", rr, err)
	}
	for _, p := range s.store.PendingMessages() {
		if p.ID == rr.ReplyID && p.NotBefore.IsZero() {
			t.Fatal("held reply has no NotBefore")
		}
	}
	ib, err = r.inbox(ctx, from(a), fleet.InboxArgs{Ticket: res.MessageID, WaitSeconds: 1})
	if err != nil || ib.Ticket.Status != fleet.AskReplied || ib.Ticket.Reply != "kept" {
		t.Fatalf("collect: %+v %v", ib.Ticket, err)
	}
	if rep, _ := s.store.Message(rr.ReplyID); rep.State != model.MsgDelivered || !rep.Inline {
		t.Fatalf("collected reply: %+v", rep)
	}

	// A ticket wait ends when the reply arrives.
	res = r.ask(ctx, from(a), fleet.AskArgs{To: "b", Text: "wait for me", WaitSeconds: new(int), HoldSeconds: 60})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: res.MessageID, Text: "here"})
	}()
	start := time.Now()
	ib, err = r.inbox(ctx, from(a), fleet.InboxArgs{Ticket: res.MessageID, WaitSeconds: 10})
	if err != nil || ib.Ticket.Status != fleet.AskReplied || time.Since(start) > 5*time.Second {
		t.Fatalf("ticket wait: %+v %v", ib.Ticket, err)
	}

	// People ask and answer through the inbox.
	pr := r.ask(ctx, sender{user: "user:jm"}, fleet.AskArgs{To: "b", Text: "name?", WaitSeconds: new(int)})
	rr, err = r.reply(ctx, from(b), fleet.ReplyArgs{MessageID: pr.MessageID, Text: "b"})
	if err != nil || rr.State != model.MsgDelivered {
		t.Fatalf("reply to a person: %+v %v", rr, err)
	}
	ua := r.ask(ctx, from(a), fleet.AskArgs{To: "user:jm", Text: "ok?", WaitSeconds: new(int)})
	if ua.Status != fleet.AskPending {
		t.Fatalf("ask a person: %+v", ua)
	}
	inbox := s.store.Messages(store.MessageFilter{ToUser: true})
	if len(inbox) != 2 { // the reply to jm's ask, and a's ask
		t.Fatalf("inbox: %+v", inbox)
	}
	if _, err := r.reply(ctx, sender{user: "user:jm"}, fleet.ReplyArgs{MessageID: ua.MessageID, Text: "yes"}); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryAndFailure(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "workbox", "b")

	msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "x", TTLSeconds: 1})
	if err != nil || msgs[0].State != model.MsgPending {
		t.Fatalf("send: %+v %v", msgs, err)
	}
	time.Sleep(1100 * time.Millisecond)
	r.poke()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := s.store.Message(msgs[0].ID); m.State == model.MsgExpired {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m, _ := s.store.Message(msgs[0].ID); m.State != model.MsgExpired {
		t.Fatalf("not expired: %+v", m)
	}
	// The sender is told (the notice is stored just after the expiry).
	var notice *model.Message
	for deadline := time.Now().Add(5 * time.Second); notice == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, m := range s.store.Messages(store.MessageFilter{Session: a.ID}) {
			if m.Kind == model.MsgSystem && m.ToSession == a.ID {
				notice = &m
			}
		}
	}
	if notice == nil || !strings.Contains(notice.Text, "expired") || notice.From != model.SystemSender {
		t.Fatalf("expiry notice: %+v", notice)
	}

	// Messages to a session that stops fail, with a notice.
	msgs, _ = r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "y"})
	_, _ = s.store.UpdateSession(b.ID, func(x *model.Session) { x.Status = model.StatusStopped })
	got := r.deliver(ctx, msgs[0])
	if got.State != model.MsgFailed {
		t.Fatalf("delivery to a stopped session: %+v", got)
	}
}

func TestCompletionNotice(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{MaxHops: 4})
	r := s.router
	p := addSession(t, s, "devbox", "p")
	c := addSession(t, s, "workbox", "c", func(x *model.Session) { x.Parent = p.ID })
	orphan := addSession(t, s, "workbox", "o")

	notices := func() []model.Message {
		var out []model.Message
		for _, m := range s.store.Messages(store.MessageFilter{Session: p.ID}) {
			if m.Kind == model.MsgDone {
				out = append(out, m)
			}
		}
		return out
	}
	r.started(c.ID, 1)
	r.onSettled(c, model.Settled{TurnStatus: "completed", LastMessage: "all done"})
	r.onSettled(orphan, model.Settled{LastMessage: "nobody cares"})
	ns := notices()
	if len(ns) != 1 || ns[0].ToSession != p.ID || ns[0].Hops != 2 || !strings.Contains(ns[0].Text, "all done") {
		t.Fatalf("notices: %+v", ns)
	}

	// The parent asked and got its answer: no notice on top.
	res := r.ask(ctx, from(p), fleet.AskArgs{To: "workbox/c", Text: "q", WaitSeconds: new(int)})
	r.onDelivered(c.ID, res.MessageID)
	if _, err := r.reply(ctx, from(c), fleet.ReplyArgs{MessageID: res.MessageID, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	r.onSettled(c, model.Settled{TurnStatus: "completed", LastMessage: "a"})
	if len(notices()) != 1 {
		t.Fatalf("notice after an answered ask: %+v", notices())
	}

	// Past the hop limit, no notice.
	r.started(c.ID, 4)
	r.onSettled(c, model.Settled{LastMessage: "deep"})
	if len(notices()) != 1 {
		t.Fatal("notice past the hop limit")
	}
}
