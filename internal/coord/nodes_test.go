// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// fakeNode connects host to s over a pipe and answers its commands.
func fakeNode(t *testing.T, s *Server, host string, answer func(wire.Cmd) wire.Result) {
	t.Helper()
	a, b := net.Pipe()
	n := &nodeConn{host: host, conn: wire.NewConn(a), raw: a, pending: map[string]chan cmdReply{}, closed: make(chan struct{}),
		reqs: make(chan struct{}, 1), hello: wire.Hello{Host: host, Adapters: []string{"codex"}}, welcomed: make(chan struct{})}
	close(n.welcomed)
	s.mu.Lock()
	s.nodes[host] = n
	s.mu.Unlock()
	go s.readNode(n)
	nc := wire.NewConn(b)
	go func() {
		for {
			env, err := nc.Recv()
			if err != nil {
				return
			}
			if env.Type == wire.TCmd {
				cmd, _ := wire.Decode[wire.Cmd](env)
				_ = nc.Send(wire.TResult, "", env.ID, answer(cmd))
			}
		}
	}()
	t.Cleanup(func() { n.close(); b.Close() })
}

// A spawn the node refuses leaves no session behind (and says so on the
// fleet stream); a "~" cwd is recorded as the node expanded it.
func TestSpawnFailureForgetsSession(t *testing.T) {
	s := testServer(t, Guards{})
	fleetCh := make(chan FleetEvent, 16)
	s.fleet[fleetCh] = struct{}{}
	refuse := true
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		if refuse {
			return wire.Result{Error: `cwd "~/nope" is not a directory on devbox`}
		}
		return wire.Result{OK: true, Data: model.Marshal(wire.SpawnResult{Cwd: "/home/jm/switchworks", Sandbox: "workspace-write"})}
	})
	ctx := context.Background()

	_, err := s.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "sw", Cwd: "~/nope"})
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("spawn: %v", err)
	}
	if all := s.store.Sessions(); len(all) != 0 {
		t.Fatalf("failed spawn kept: %+v", all)
	}
	var created, removed string
	for len(fleetCh) > 0 {
		ev := <-fleetCh
		switch ev.Type {
		case "session":
			created = ev.Data.(apiclient.SessionView).ID
		case "session_removed":
			removed = ev.Data.(map[string]string)["id"]
		}
	}
	if removed == "" || removed != created {
		t.Fatalf("published %q, removed %q", created, removed)
	}

	refuse = false
	sess, err := s.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "sw", Cwd: "~/switchworks"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Cwd != "/home/jm/switchworks" {
		t.Fatalf("cwd %q", sess.Cwd)
	}
	if all := s.store.Sessions(); len(all) != 1 || all[0].Path != "devbox/sw" {
		t.Fatalf("sessions %+v", all)
	}
	// Only a "~" cwd takes the node's word.
	sess, err = s.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "abs", Cwd: "/srv/x"})
	if err != nil || sess.Cwd != "/srv/x" {
		t.Fatalf("absolute cwd: %q %v", sess.Cwd, err)
	}
}

// A connected host's LastSeen follows its traffic; a replaced connection's
// does not count.
func TestTouchHost(t *testing.T) {
	s := testServer(t, Guards{})
	start := time.Now().Add(-time.Hour)
	if err := s.store.UpsertHost(model.Host{ID: "devbox", LastSeen: start}); err != nil {
		t.Fatal(err)
	}
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result { return wire.Result{OK: true} })
	n, _ := s.node("devbox")
	s.touchHost(&nodeConn{host: "devbox"}, time.Now())
	if h, _ := s.store.Host("devbox"); !h.LastSeen.Equal(start) {
		t.Fatalf("stale connection moved LastSeen to %v", h.LastSeen)
	}
	now := time.Now()
	s.touchHost(n, now)
	if h, _ := s.store.Host("devbox"); !h.LastSeen.Equal(now) {
		t.Fatalf("LastSeen %v, want %v", h.LastSeen, now)
	}
}

// Sessions orphaned by failed spawns before failures were forgotten are
// removed at startup; an errored session a harness registered stays.
func TestPurgeFailedSpawns(t *testing.T) {
	s := testServer(t, Guards{})
	orphan := addSession(t, s, "antec", "switchworks", func(x *model.Session) { x.Status = model.StatusError })
	real := addSession(t, s, "antec", "crashed", func(x *model.Session) {
		x.Status = model.StatusError
		x.NativeRef = &model.NativeRef{Harness: "agy", ID: "c1"}
	})
	live := addSession(t, s, "antec", "live")
	s.purgeFailedSpawns()
	if _, ok := s.store.Session(orphan.ID); ok {
		t.Fatal("failed spawn kept")
	}
	for _, sess := range []model.Session{real, live} {
		if _, ok := s.store.Session(sess.ID); !ok {
			t.Fatalf("%s removed", sess.Path)
		}
	}
}

// A session deleted from the store whose event log could not be removed is
// still forgotten by the fleet views.
func TestForgetSessionLogNotRemoved(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	s := testServer(t, Guards{})
	dir := t.TempDir()
	st, err := store.OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.store = st
	fleetCh := make(chan FleetEvent, 16)
	s.fleet[fleetCh] = struct{}{}
	sess := addSession(t, s, "devbox", "sw", func(x *model.Session) { x.ID = ulid.New() })
	if _, err := s.store.AppendEvents([]model.Event{{ID: ulid.New(), SessionID: sess.ID, Seq: 1, TS: time.Now(),
		Kind: model.KindWarning, Payload: model.Marshal(model.ErrorEvent{Message: "x"})}}); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(dir, "events")
	_ = os.Chmod(events, 0o500)
	s.forgetSession(sess.ID)
	_ = os.Chmod(events, 0o755)
	if _, ok := s.store.Session(sess.ID); ok {
		t.Fatal("session kept")
	}
	for {
		select {
		case ev := <-fleetCh:
			if ev.Type == "session_removed" {
				return
			}
		default:
			t.Fatal("session_removed not published")
		}
	}
}

// A node is registered before its welcome is sent. A command in that window
// waits for the welcome: sent first, it would end the node's handshake.
func TestCommandWaitsForWelcome(t *testing.T) {
	s := pruneServer(t)
	a, b := net.Pipe()
	n := &nodeConn{host: "devbox", conn: wire.NewConn(a), raw: a, pending: map[string]chan cmdReply{}, closed: make(chan struct{}),
		reqs: make(chan struct{}, 1), welcomed: make(chan struct{})}
	s.mu.Lock()
	s.nodes["devbox"] = n
	s.mu.Unlock()
	go s.readNode(n)
	t.Cleanup(func() { n.close(); b.Close() })

	got := make(chan string, 4)
	nc := wire.NewConn(b)
	go func() {
		for {
			env, err := nc.Recv()
			if err != nil {
				return
			}
			got <- env.Type
			if env.Type == wire.TCmd {
				_ = nc.Send(wire.TResult, "", env.ID, wire.Result{OK: true})
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := s.command(context.Background(), "devbox", wire.Cmd{Op: wire.OpStop}, 5*time.Second)
		done <- err
	}()
	select {
	case typ := <-got:
		t.Fatalf("%q sent before the welcome", typ)
	case <-time.After(200 * time.Millisecond):
	}
	if err := n.conn.Send(wire.TWelcome, "", "", wire.Welcome{}); err != nil {
		t.Fatal(err)
	}
	close(n.welcomed)
	if typ := <-got; typ != wire.TWelcome {
		t.Fatalf("first envelope %q, want the welcome", typ)
	}
	if typ := <-got; typ != wire.TCmd {
		t.Fatalf("then %q, want the command", typ)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A node that goes away before its welcome fails the command at once.
	n2 := &nodeConn{host: "laptop", pending: map[string]chan cmdReply{}, closed: make(chan struct{}), welcomed: make(chan struct{})}
	s.mu.Lock()
	s.nodes["laptop"] = n2
	s.mu.Unlock()
	close(n2.closed)
	if _, err := s.command(context.Background(), "laptop", wire.Cmd{Op: wire.OpStop}, 5*time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("command to a node closed before its welcome: %v", err)
	}
}
