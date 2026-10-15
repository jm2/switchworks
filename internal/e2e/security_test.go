// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/node"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/wire"
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
