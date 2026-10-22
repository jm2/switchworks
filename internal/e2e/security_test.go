// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/node"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/wire"
)

// A node with a genuine certificate for host "a" must not be able to write
// outside the store, write into host b's logs, or take over b's paths.
func TestNodeCannotForgeSessions(t *testing.T) {
	base := t.TempDir()
	coordDir := filepath.Join(base, "coord")
	c := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer c.stop()
	api := apiClient(t, coordDir)
	ctx := context.Background()

	// Enroll host a and host b.
	dirs := map[string]string{}
	for _, h := range []string{"a", "b"} {
		tok, err := api.EnrollToken(ctx, h, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(base, h)
		if _, err := node.Enroll(ctx, d, tok, false); err != nil {
			t.Fatal(err)
		}
		dirs[h] = d
	}
	// A victim session on host b with one event already stored.
	victim := model.Session{ID: "01JBBBBBBBBBBBBBBBBBBBBBBB", Path: "b/lead", Host: "b", Name: "lead", Status: model.StatusStopped}
	if err := c.st.CreateSession(victim); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.AppendEvents([]model.Event{{ID: "e1", SessionID: victim.ID, Seq: 1, Kind: "item.completed",
		Payload: model.Marshal(model.Item{ItemType: "agentMessage", Text: "genuine b output"})}}); err != nil {
		t.Fatal(err)
	}

	// Host a dials with its genuine certificate.
	rd := func(n string) []byte { b, _ := os.ReadFile(filepath.Join(dirs["a"], n)); return b }
	cfg, err := pki.ClientTLS(rd("ca.crt"), rd("node.crt"), rd("node.key"), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", c.nodeAdd, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wc := wire.NewConn(conn)
	// Hello adopting a session that squats on b's path, with an id that sorts last.
	if err := wc.Send(wire.THello, "", "", wire.Hello{Protocol: 1, Host: "a", Adapters: []string{"codex"},
		Sessions: []wire.SessionState{{ID: "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"[:26], Path: "b/lead", Harness: "codex", Status: "idle"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Recv(); err != nil { // welcome
		t.Fatal(err)
	}
	evs := []model.Event{
		{ID: "x1", SessionID: "../../../../pwned", Seq: 1, Kind: "note", Payload: model.Marshal(map[string]string{"hello": "outside the store"})},
		{ID: "x2", SessionID: "x/../" + victim.ID, Seq: 2, Kind: "item.completed",
			Payload: model.Marshal(model.Item{ItemType: "agentMessage", Text: "INJECTED BY HOST a"})},
	}
	if err := wc.Send(wire.TEvents, "", "", wire.Events{Events: evs}); err != nil {
		t.Fatal(err)
	}
	for {
		env, err := wc.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if env.Type == wire.TAck {
			t.Logf("ack: %s", env.P)
			break
		}
	}
	// 1. Nothing written outside the coordinator's data dir.
	for _, dir := range []string{base, filepath.Dir(base), coordDir} {
		if m, _ := filepath.Glob(filepath.Join(dir, "*.jsonl")); len(m) > 0 {
			t.Errorf("event file outside the store: %v", m)
			for _, f := range m {
				os.Remove(f)
			}
		}
	}
	// 2. The victim's log is untouched.
	b, _ := os.ReadFile(filepath.Join(coordDir, "data", "events", victim.ID+".jsonl"))
	if strings.Contains(string(b), "INJECTED BY HOST a") {
		t.Errorf("host a wrote into host b's log:\n%s", b)
	}
	// 3. b/lead still belongs to host b.
	if s, ok := c.st.SessionByPath("b/lead"); !ok || s.Host != "b" {
		t.Errorf("b/lead resolves to %+v", s)
	}
	conn.Close()
}

// Enrolling a host again replaces its certificate, and removing a host
// revokes it: the old certificate no longer connects.
func TestNodeCertificateRevocation(t *testing.T) {
	base := t.TempDir()
	coordDir := filepath.Join(base, "coord")
	c := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer c.stop()
	api := apiClient(t, coordDir)
	ctx := context.Background()

	enroll := func(dir string) {
		t.Helper()
		tok, err := api.EnrollToken(ctx, "a", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := node.Enroll(ctx, dir, tok, true); err != nil {
			t.Fatal(err)
		}
	}
	// connects reports whether a node with dir's identity gets a welcome.
	connects := func(dir string) bool {
		t.Helper()
		rd := func(n string) []byte { b, _ := os.ReadFile(filepath.Join(dir, n)); return b }
		cfg, err := pki.ClientTLS(rd("ca.crt"), rd("node.crt"), rd("node.key"), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := tls.Dial("tcp", c.nodeAdd, cfg)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		wc := wire.NewConn(conn)
		if wc.Send(wire.THello, "", "", wire.Hello{Protocol: wire.ProtocolVersion, Host: "a"}) != nil {
			return false
		}
		env, err := wc.Recv()
		return err == nil && env.Type == wire.TWelcome
	}

	first, second := filepath.Join(base, "n1"), filepath.Join(base, "n2")
	enroll(first)
	if !connects(first) {
		t.Fatal("enrolled node cannot connect")
	}
	enroll(second)
	if connects(first) {
		t.Fatal("the replaced certificate still connects")
	}
	if !connects(second) {
		t.Fatal("the new certificate cannot connect")
	}
	if err := api.RemoveHost(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if connects(second) {
		t.Fatal("a removed host still connects")
	}
}

// A connection that passed the certificate check before `hosts rm` or a
// re-enroll, and sends its hello only afterwards, must not bring the old
// certificate back.
func TestNodeRevocationRaces(t *testing.T) {
	base := t.TempDir()
	coordDir := filepath.Join(base, "coord")
	c := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer c.stop()
	api := apiClient(t, coordDir)
	ctx := context.Background()
	enroll := func(dir string) {
		t.Helper()
		tok, err := api.EnrollToken(ctx, "a", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := node.Enroll(ctx, dir, tok, true); err != nil {
			t.Fatal(err)
		}
	}
	dial := func(dir string) *tls.Conn {
		t.Helper()
		rd := func(n string) []byte { b, _ := os.ReadFile(filepath.Join(dir, n)); return b }
		cfg, err := pki.ClientTLS(rd("ca.crt"), rd("node.crt"), rd("node.key"), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := tls.Dial("tcp", c.nodeAdd, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	welcomed := func(conn *tls.Conn) bool {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		wc := wire.NewConn(conn)
		if wc.Send(wire.THello, "", "", wire.Hello{Protocol: wire.ProtocolVersion, Host: "a"}) != nil {
			return false
		}
		env, err := wc.Recv()
		return err == nil && env.Type == wire.TWelcome
	}

	// Re-enroll while an old-certificate connection waits.
	old, fresh := filepath.Join(base, "old"), filepath.Join(base, "new")
	enroll(old)
	pending := dial(old)
	time.Sleep(200 * time.Millisecond) // past the first certificate check
	enroll(fresh)
	if welcomed(pending) {
		t.Fatal("a connection with the replaced certificate completed after re-enrolling")
	}
	if welcomed(dial(old)) {
		t.Fatal("the replaced certificate connects again")
	}
	if !welcomed(dial(fresh)) {
		t.Fatal("the new certificate was locked out")
	}

	// Remove while a connection waits.
	pending = dial(fresh)
	time.Sleep(200 * time.Millisecond)
	if err := api.RemoveHost(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if welcomed(pending) {
		t.Fatal("a connection completed after the host was removed")
	}
	if _, ok := c.st.Host("a"); ok {
		t.Fatal("the removed host came back")
	}
}

// An agent that can re-point symlinks must not get a descendant working
// outside its tree: a child's cwd is recorded resolved (so grandchildren
// are rooted at the real path), a writing parent may not spawn below its
// own cwd, and a cwd swapped for a symlink after spawn fails the next turn.
func TestSpawnCwdSymlinkSwaps(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer co.stop()
	c := apiClient(t, coordDir)
	_, _, nd := enrollHost(t, c, "devbox")
	defer func() { nd.stop(t) }()
	waitOnline(t, c, "devbox")

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj, outside := filepath.Join(base, "proj"), filepath.Join(base, "outside")
	real, sub := filepath.Join(proj, "real"), filepath.Join(proj, "sub")
	for _, d := range []string{real, sub, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(proj, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	spawn := func(name, sandbox string) (apiclient.SessionView, *watcher) {
		t.Helper()
		s, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: name, Cwd: proj, ApprovalPolicy: "never", Sandbox: sandbox})
		if err != nil {
			t.Fatal(err)
		}
		waitStatus(t, c, s.ID, model.StatusIdle)
		return s, watch(c, s.ID)
	}
	say := func(id, text string) {
		t.Helper()
		if _, err := c.Send(ctx, id, text, "queue", ""); err != nil {
			t.Fatal(err)
		}
	}

	// A workspace-write parent may spawn only in its own cwd.
	w, ww := spawn("writer", "workspace-write")
	defer ww.cancel()
	say(w.ID, call("fleet_spawn", map[string]any{"cwd": link, "name": "c1"}))
	ww.waitFor(t, "spawn below a writable cwd refused", 0, agentSaidAll("tool fleet_spawn: Error:", "may spawn only there"))

	// A read-only parent may spawn below its cwd, through a link; the child
	// is recorded at the real path, so re-pointing the link afterwards does
	// not move where the child's own children go.
	r, wr := spawn("reader", "read-only")
	defer wr.cancel()
	say(r.ID, call("fleet_spawn", map[string]any{"cwd": link, "name": "c2"}))
	i := wr.waitFor(t, "spawned via link", 0, agentSaidAll("tool fleet_spawn:", `"path": "devbox/c2"`))
	c2, err := c.Resolve(ctx, "devbox/c2")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Cwd != real {
		t.Fatalf("child recorded at %q, want the resolved %q", c2.Cwd, real)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	w2 := watch(c, c2.ID)
	defer w2.cancel()
	waitStatus(t, c, c2.ID, model.StatusIdle)
	say(c2.ID, call("fleet_spawn", map[string]any{"name": "g2"}))
	w2.waitFor(t, "grandchild spawned", 0, agentSaidAll("tool fleet_spawn:", `"path": "devbox/g2"`))
	if g2, err := c.Resolve(ctx, "devbox/g2"); err != nil || g2.Cwd != real {
		t.Fatalf("grandchild at %q (%v), want %q", g2.Cwd, err, real)
	}

	// A child whose cwd is swapped for a symlink after spawn gets no
	// further turns there.
	say(r.ID, call("fleet_spawn", map[string]any{"cwd": sub, "name": "c3"}))
	wr.waitFor(t, "spawned in sub", i, agentSaidAll("tool fleet_spawn:", `"path": "devbox/c3"`))
	c3, err := c.Resolve(ctx, "devbox/c3")
	if err != nil {
		t.Fatal(err)
	}
	w3 := watch(c, c3.ID)
	defer w3.cancel()
	waitStatus(t, c, c3.ID, model.StatusIdle)
	if err := os.Rename(sub, filepath.Join(proj, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	// The refusal comes as message.failed, or as the send's own error when
	// it fails before the coordinator answers.
	refused := fmt.Sprintf("cwd %q", sub)
	if _, err := c.Send(ctx, c3.ID, "write here", "queue", ""); err != nil {
		if !strings.Contains(err.Error(), refused) {
			t.Fatal(err)
		}
	} else {
		w3.waitFor(t, "turn refused after the swap", 0, func(e model.Event) bool {
			var p struct {
				Error string `json:"error"`
			}
			return e.Kind == model.KindMessageFailed && json.Unmarshal(e.Payload, &p) == nil &&
				strings.Contains(p.Error, refused)
		})
	}
	if countAgent(w3, agentSaid("write here")) != 0 {
		t.Fatal("the turn ran in the swapped cwd")
	}
}
