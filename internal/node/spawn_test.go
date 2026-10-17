// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// failingAdapter records the spec it was asked to start, emits an event,
// then fails.
type failingAdapter struct{ spec adapter.StartSpec }

func (f *failingAdapter) Name() string { return "fake" }
func (f *failingAdapter) Start(_ context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	f.spec = spec
	sink(adapter.Event{Kind: model.KindWarning, Payload: model.ErrorEvent{Message: "starting"}})
	return nil, errors.New("harness exploded")
}
func (f *failingAdapter) Resume(context.Context, model.NativeRef, adapter.StartSpec, adapter.Sink) (adapter.Handle, error) {
	return nil, errors.New("no")
}
func (f *failingAdapter) Close() error { return nil }

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for in, want := range map[string]string{
		"~":              home,
		"~/":             home,
		"~/switchworks":  filepath.Join(home, "switchworks"),
		"~bob/x":         "~bob/x",
		"/srv/~/x":       "/srv/~/x",
		"relative/~/dir": "relative/~/dir",
	} {
		if got, err := expandHome(in); err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// A spawn under "~" runs in the expanded directory; one whose harness fails
// to start leaves nothing behind on the node.
func TestSpawnExpandsHomeAndForgetsFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	proj := filepath.Join(home, "proj")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	fa := &failingAdapter{}
	n := &Node{dir: dir, logf: t.Logf, adapters: map[string]adapter.Adapter{"fake": fa}, sessions: map[string]*session{}}
	cmd := func(cwd string) *wire.SpawnCmd {
		return &wire.SpawnCmd{Session: model.Session{ID: ulid.New(), Path: "devbox/a"}, Spec: model.SpawnSpec{Harness: "fake", Cwd: cwd}}
	}

	c := cmd("~/proj")
	if _, err := n.spawn(context.Background(), c); err == nil || !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("spawn: %v", err)
	}
	if fa.spec.Cwd != proj {
		t.Fatalf("harness started in %q, want %q", fa.spec.Cwd, proj)
	}
	if len(n.sessionList()) != 0 {
		t.Fatal("failed session kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "spool", c.Session.ID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("spool kept: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var st struct{ Sessions []json.RawMessage }
	if json.Unmarshal(b, &st) != nil || len(st.Sessions) != 0 {
		t.Fatalf("state.json: %s", b)
	}
	if h := n.hello(); len(h.Sessions) != 0 {
		t.Fatalf("hello reports %+v", h.Sessions)
	}

	// "~user" is not expanded, and fails as a missing directory.
	if _, err := n.spawn(context.Background(), cmd("~nobody/proj")); err == nil || !strings.Contains(err.Error(), `"~nobody/proj" is not a directory`) {
		t.Fatalf("~user spawn: %v", err)
	}
}
