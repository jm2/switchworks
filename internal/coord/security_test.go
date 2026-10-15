// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/store"
)

// A spawn loop (an agent spawns a child, hears it finished, spawns another)
// climbs the hop count and stops at the limit: the spawn prompt is a routed
// message carrying the parent's hops, not an unknown one that resets them.
func TestSpawnLoopHitsHopLimit(t *testing.T) {
	s := testServer(t, Guards{MaxHops: 6})
	r := s.router
	ctx := context.Background()
	p := addSession(t, s, "devbox", "p")
	for i := 0; i < 20; i++ {
		hops := r.hopsFor(from(p))
		if hops > r.guards.MaxHops {
			return // fleetSpawn refuses here
		}
		c := addSession(t, s, "devbox", "c"+string(rune('a'+i)), func(x *model.Session) { x.Parent = p.ID })
		r.started(c.ID, hops)
		msgs, err := r.send(ctx, from(p), model.MsgNotify, fleet.SendArgs{To: c.ID, Text: "go"})
		if err != nil {
			return // refused by the hop limit: also fine
		}
		r.onDelivered(c.ID, msgs[0].ID)
		r.onSettled(c, model.Settled{TurnStatus: "completed", LastMessage: "done"})
		var done model.Message
		for _, m := range s.store.Messages(store.MessageFilter{Session: p.ID, Limit: 1000}) {
			if m.Kind == model.MsgDone && m.ToSession == p.ID && m.ID > done.ID {
				done = m
			}
		}
		if done.ID == "" {
			return // the completion notice was withheld at the limit
		}
		r.onDelivered(p.ID, done.ID)
	}
	t.Fatal("a spawn loop ran 20 rounds without reaching the hop limit")
}

func TestAgentSpawnConfinement(t *testing.T) {
	s := testServer(t, Guards{})
	parent := addSession(t, s, "devbox", "lead", func(x *model.Session) {
		x.Cwd, x.ApprovalPolicy, x.Sandbox = "/home/dev/src/app", "untrusted", "workspace-write"
	})
	spawn := func(spec model.SpawnSpec) (model.SpawnSpec, error) {
		err := s.agentSpawn(from(parent), &spec, 1)
		return spec, err
	}

	got, err := spawn(model.SpawnSpec{Prompt: "go", Cwd: "/home/dev/src/app/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "devbox" || got.Harness != "codex" || got.CwdRoot != "/home/dev/src/app" ||
		got.ApprovalPolicy != "untrusted" || got.Sandbox != "workspace-write" || got.Prompt != "" {
		t.Fatalf("child spec not confined to its parent: %+v", got)
	}
	if got, _ := spawn(model.SpawnSpec{}); got.Cwd != "/home/dev/src/app" {
		t.Fatalf("default cwd %q", got.Cwd)
	}
	for _, c := range []struct {
		spec model.SpawnSpec
		err  string
	}{
		{model.SpawnSpec{Host: "buildbox"}, "only on their own host"},
		{model.SpawnSpec{Harness: "goose"}, "only their own harness"},
		{model.SpawnSpec{Cwd: "/home/dev"}, "outside"},
		{model.SpawnSpec{Cwd: "/home/dev/src/app/../../.."}, "outside"},
		{model.SpawnSpec{Cwd: "/home/dev/src/application"}, "outside"},
		{model.SpawnSpec{Cwd: "relative/path"}, "outside"},
		{model.SpawnSpec{Labels: map[string]string{"owner.role": "reviewer"}}, "reserved"},
		{model.SpawnSpec{Labels: map[string]string{"host": "x"}}, "reserved"},
	} {
		if _, err := spawn(c.spec); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: got %v, want %q", c.spec, err, c.err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := spawn(model.SpawnSpec{}); err != nil {
			if !strings.Contains(err.Error(), "spawn rate limit") {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("no spawn rate limit")
}

// A parent whose policies are unknown hands its child the strictest ones,
// not the node's defaults.
func TestAgentSpawnUnknownPolicies(t *testing.T) {
	s := testServer(t, Guards{})
	parent := addSession(t, s, "devbox", "old", func(x *model.Session) { x.Cwd = "/w" })
	spec := model.SpawnSpec{}
	if err := s.agentSpawn(from(parent), &spec, 1); err != nil {
		t.Fatal(err)
	}
	if spec.ApprovalPolicy != "untrusted" || spec.Sandbox != "read-only" {
		t.Fatalf("child policies %q %q", spec.ApprovalPolicy, spec.Sandbox)
	}
}

func TestAgentSpawnGuardsWiden(t *testing.T) {
	s := testServer(t, Guards{SpawnHosts: []string{"*"}, SpawnHarnesses: []string{"goose"}, SpawnCwdRoots: []string{"/srv/work"}})
	parent := addSession(t, s, "devbox", "lead", func(x *model.Session) { x.Cwd, x.Sandbox = "/home/dev/app", "danger-full-access" })
	spec := model.SpawnSpec{Host: "buildbox", Harness: "goose", Cwd: "/srv/work/x"}
	if err := s.agentSpawn(from(parent), &spec, 1); err != nil {
		t.Fatal(err)
	}
	if spec.CwdRoot != "/srv/work" {
		t.Fatalf("cwd root %q", spec.CwdRoot)
	}
	spec = model.SpawnSpec{Harness: "agy"}
	if err := s.agentSpawn(from(parent), &spec, 1); err == nil {
		t.Fatal("harness outside spawn_harnesses allowed")
	}
}

func TestPathWithin(t *testing.T) {
	for _, c := range []struct {
		root, dir string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b/", "/a/b/c/", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b/../c", false},
		{"/", "/anything", true},
		{`C:\src\app`, `c:\SRC\app\x`, true},
		{`C:\src\app`, `D:\src\app`, false},
		{"/a", "a", false},
	} {
		if got := pathWithin(c.root, c.dir); got != c.want {
			t.Errorf("pathWithin(%q, %q) = %v", c.root, c.dir, got)
		}
	}
}

func TestAgentMessagingLimits(t *testing.T) {
	s := testServer(t, Guards{MaxBroadcast: 2})
	r := s.router
	ctx := context.Background()
	a := addSession(t, s, "devbox", "a")
	child := addSession(t, s, "devbox", "child", func(x *model.Session) { x.Parent = a.ID })
	other := addSession(t, s, "devbox", "other")
	addSession(t, s, "devbox", "third")

	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: other.ID, Text: "x", Mode: model.ModeInterrupt}); err == nil {
		t.Fatal("an agent interrupted a session it did not spawn")
	}
	if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: child.ID, Text: "x", Mode: model.ModeSteer}); err != nil {
		t.Fatalf("steering its own child: %v", err)
	}
	if _, err := r.send(ctx, sender{user: "user:jm"}, model.MsgNotify, fleet.SendArgs{To: other.ID, Text: "x", Mode: model.ModeInterrupt}); err != nil {
		t.Fatalf("a person interrupting: %v", err)
	}
	if _, err := r.send(ctx, from(a), model.MsgBroadcast, fleet.SendArgs{To: "@harness=codex", Text: "x"}); err == nil ||
		!strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("broadcast cap: %v", err)
	}

	// fleet_status shows output only to the session's family.
	st, err := s.fleetStatus(from(other), fleet.StatusArgs{Session: child.ID})
	if err != nil || st.LastMessage != "" {
		t.Fatalf("status for a stranger: %+v %v", st, err)
	}

	// A ticket is readable only by the session that asked, not by a later
	// session reusing its path.
	res := r.ask(ctx, from(a), fleet.AskArgs{To: other.ID, Text: "q?", WaitSeconds: new(int)})
	if res.MessageID == "" {
		t.Fatalf("ask: %+v", res)
	}
	impostor := model.Session{ID: "s-devbox-a2", Path: a.Path, Host: "devbox", Name: "a"}
	if _, err := r.inbox(ctx, from(impostor), fleet.InboxArgs{Ticket: res.MessageID}); err == nil {
		t.Fatal("another session read the ticket through a reused path")
	}
	if _, err := r.inbox(ctx, from(a), fleet.InboxArgs{Ticket: res.MessageID}); err != nil {
		t.Fatalf("asker reading its ticket: %v", err)
	}
}

// One client holding many connections open is turned away past its share
// and does not block anyone else.
func TestConnectionLimits(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := limitListener(inner, 100, 3)
	defer ln.Close()
	accepted := make(chan net.Conn, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var held []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		held = append(held, c)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(accepted); n != 3 {
		t.Fatalf("accepted %d connections from one address, want 3", n)
	}
	// Closing one frees a slot.
	(<-accepted).Close()
	c, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("a freed slot was not reused")
	}
	_ = held
}

// Made-up user: names and many recipients do not get an agent past its
// budget: people share one bucket, and every message counts against the
// sender's total.
func TestAgentSendBudget(t *testing.T) {
	s := testServer(t, Guards{SendPerMinute: 2, OutPerMinute: 5})
	r := s.router
	ctx := context.Background()
	a := addSession(t, s, "devbox", "a")
	sent := 0
	for i := 0; i < 50; i++ {
		if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "user:u" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Text: "x"}); err == nil {
			sent++
		}
	}
	if sent != 2 {
		t.Fatalf("%d messages to invented people got through, want 2", sent)
	}
	for i := 0; i < 10; i++ {
		addSession(t, s, "devbox", "t"+string(rune('a'+i)))
	}
	sent = 0
	for i := 0; i < 10; i++ {
		if _, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/t" + string(rune('a'+i)), Text: "x"}); err == nil {
			sent++
		}
	}
	if sent != 3 { // 5 in all, 2 already used
		t.Fatalf("%d messages to distinct sessions got through, want 3", sent)
	}
	s.router.tidy(time.Now().Add(10 * time.Minute))
	s.router.mu.Lock()
	n := len(s.router.buckets)
	s.router.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d buckets left after tidying", n)
	}
}

// A sandboxed agent cannot start a harness that ignores sandboxes, even
// where spawn_harnesses allows that harness, unless the guards say so.
func TestAgentSpawnKeepsSandbox(t *testing.T) {
	s := testServer(t, Guards{SpawnHarnesses: []string{"goose"}})
	parent := addSession(t, s, "devbox", "lead", func(x *model.Session) {
		x.Cwd, x.ApprovalPolicy, x.Sandbox = "/w", "never", "read-only"
	})
	spec := model.SpawnSpec{Harness: "goose"}
	if err := s.agentSpawn(from(parent), &spec, 1); err == nil || !strings.Contains(err.Error(), "does not enforce a sandbox") {
		t.Fatalf("read-only codex agent spawned goose: %v", err)
	}
	s.router.guards.AllowUnsandboxedSpawn = true
	spec = model.SpawnSpec{Harness: "goose"}
	if err := s.agentSpawn(from(parent), &spec, 1); err != nil {
		t.Fatalf("with allow_unsandboxed_spawn: %v", err)
	}
	// A goose agent (already unsandboxed) may spawn goose.
	s.router.guards.AllowUnsandboxedSpawn = false
	g := addSession(t, s, "devbox", "g", func(x *model.Session) { x.Cwd, x.Harness, x.Sandbox = "/w", "goose", "read-only" })
	spec = model.SpawnSpec{}
	if err := s.agentSpawn(from(g), &spec, 1); err != nil {
		t.Fatalf("goose spawning goose: %v", err)
	}
}
