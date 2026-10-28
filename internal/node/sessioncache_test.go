// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/adapter/codexad"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

func TestCodexCache(t *testing.T) {
	both := []string{"XDG_CACHE_HOME", "CCACHE_DIR"}
	for name, c := range map[string]struct {
		node, toml string
		want       codexad.Cache
		logs       bool
	}{
		"defaults": {`{}`, ``, codexad.Cache{Env: both, RootsKnown: true}, false},
		"config.toml roots": {`{}`, "[sandbox_workspace_write]\nwritable_roots = [\"/srv/a\"]\n",
			codexad.Cache{Env: both, Roots: []string{"/srv/a"}, RootsKnown: true}, false},
		"codex.config roots win over config.toml": {
			`{"codex": {"config": {"sandbox_workspace_write": {"writable_roots": ["/srv/b"]}}}}`,
			"[sandbox_workspace_write]\nwritable_roots = [\"/srv/a\"]\n",
			codexad.Cache{Env: both, Roots: []string{"/srv/b"}, RootsKnown: true}, false},
		"codex.args roots": {`{"codex": {"args": ["-c", "sandbox_workspace_write.writable_roots=['/srv/c']", "app-server"]}}`, ``,
			codexad.Cache{Env: both, Roots: []string{"/srv/c"}, RootsKnown: true}, false},
		"args set the whole table": {`{"codex": {"args": ["-c", "sandbox_workspace_write={writable_roots=[]}", "app-server"]}}`, ``,
			codexad.Cache{Env: both}, true},
		"config.toml roots unreadable": {`{}`, "sandbox_workspace_write = { writable_roots = [\"/a\"] }\n",
			codexad.Cache{Env: both}, true},
		"codex.config sets CCACHE_DIR": {`{"codex": {"config": {"shell_environment_policy": {"set": {"CCACHE_DIR": "/mine"}}}}}`, ``,
			codexad.Cache{Env: []string{"XDG_CACHE_HOME"}, RootsKnown: true}, false},
		"session_env sets XDG_CACHE_HOME": {`{"codex": {"session_env": {"set": {"xdg_cache_home": "/mine"}}}}`, ``,
			codexad.Cache{Env: []string{"CCACHE_DIR"}, RootsKnown: true}, false},
		"config.toml sets CCACHE_DIR": {`{}`, "[shell_environment_policy.set]\nCCACHE_DIR = \"/mine\"\n",
			codexad.Cache{Env: []string{"XDG_CACHE_HOME"}, RootsKnown: true}, false},
		"args set the whole env table": {`{"codex": {"args": ["-c", "shell_environment_policy.set={A=\"1\"}", "app-server"]}}`, ``,
			codexad.Cache{RootsKnown: true}, false},
		"profile": {`{"codex": {"permissions": "swk"}}`, ``, codexad.Cache{Env: both, RootsKnown: true, ProfileOK: true}, false},
		"profile filesystem set with -c": {
			`{"codex": {"permissions": "swk", "args": ["-c", "permissions.swk.filesystem={}", "app-server"]}}`, ``,
			codexad.Cache{Env: both, RootsKnown: true}, true},
		"another profile set with -c": {
			`{"codex": {"permissions": "swk", "args": ["-c", "permissions.other.network.enabled=true", "app-server"]}}`, ``,
			codexad.Cache{Env: both, RootsKnown: true, ProfileOK: true}, false},
	} {
		cfg := nodeConfig(t, c.node)
		over, err := codexConfig(cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var logs []string
		got := codexCache(cfg, over, []byte(c.toml), func(f string, a ...any) { logs = append(logs, f) })
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
		if (len(logs) > 0) != c.logs {
			t.Errorf("%s: logs %q", name, logs)
		}
	}
}

// specAdapter records the specs it starts and resumes sessions with.
type specAdapter struct {
	policyAdapter
	started, resumed []adapter.StartSpec
}

func (a *specAdapter) Start(ctx context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	a.started = append(a.started, spec)
	return a.policyAdapter.Start(ctx, spec, sink)
}

func (a *specAdapter) Resume(_ context.Context, _ model.NativeRef, spec adapter.StartSpec, _ adapter.Sink) (adapter.Handle, error) {
	a.resumed = append(a.resumed, spec)
	return &policyHandle{}, nil
}

// A Codex session gets its own cache directory, kept across a resume and
// removed when it stops, fails to start or is forgotten; other harnesses
// get none, and a node start sweeps the directories of sessions it no
// longer has (and nothing not named like a session).
func TestSessionCacheLifecycle(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(t.TempDir(), "caches")
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	codexA, fakeA := &specAdapter{}, &specAdapter{}
	var cfg Config
	cfg.SessionCache.Dir = root
	n := &Node{dir: dir, cfg: cfg, logf: t.Logf, sessions: map[string]*session{},
		adapters: map[string]adapter.Adapter{"codex": codexA, "fake": fakeA}}
	t.Cleanup(func() { closeSpools(n) })
	spawn := func(harnessName string) string {
		t.Helper()
		id := ulid.New()
		if _, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: id, Path: "devbox/" + strings.ToLower(id[20:])},
			Spec: model.SpawnSpec{Harness: harnessName, Cwd: t.TempDir()}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := spawn("codex")
	realRoot, _ := filepath.EvalSymlinks(root)
	want := filepath.Join(realRoot, id)
	if got := codexA.started[0].CacheDir; got != want {
		t.Fatalf("cache dir %q, want %q", got, want)
	}
	fi, err := os.Stat(want)
	if err != nil || !fi.IsDir() {
		t.Fatalf("cache dir: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode %v", fi.Mode().Perm())
	}
	if other := spawn("fake"); fakeA.started[0].CacheDir != "" {
		t.Fatalf("a %s session got a cache dir", other)
	}

	// Kept, with what the tools put there, across a resume.
	if err := os.WriteFile(filepath.Join(want, "ccache.conf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := n.session(id)
	s.mu.Lock()
	s.rec.NativeRef = &model.NativeRef{Harness: "codex", ID: "t1"}
	s.mu.Unlock()
	n.resume(s)
	if len(codexA.resumed) != 1 || codexA.resumed[0].CacheDir != want {
		t.Fatalf("resumed with %+v", codexA.resumed)
	}
	if _, err := os.Stat(filepath.Join(want, "ccache.conf")); err != nil {
		t.Fatalf("cache lost on resume: %v", err)
	}

	// Stopped: gone.
	if err := n.stop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("cache dir kept after stop: %v", err)
	}

	// Forgotten: gone too (here it came back after the stop).
	if err := os.Mkdir(want, 0o700); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.unacked = nil // the coordinator has them all
	s.mu.Unlock()
	n.forgetStopped()
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("cache dir kept after forget: %v", err)
	}

	// A failed start leaves none.
	n.adapters["codex"] = &failingAdapter{}
	fid := ulid.New()
	if _, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: fid, Path: "devbox/f"},
		Spec: model.SpawnSpec{Harness: "codex", Cwd: t.TempDir()}}); err == nil {
		t.Fatal("spawn worked")
	}
	if _, err := os.Stat(filepath.Join(root, fid)); !os.IsNotExist(err) {
		t.Fatalf("cache dir kept after a failed start: %v", err)
	}

	// The sweep keeps live sessions' and anything else in the root.
	live, stale := ulid.New(), ulid.New()
	for _, d := range []string{live, stale, "notes"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n.sweepCaches(map[string]bool{live: true})
	for d, kept := range map[string]bool{live: true, stale: false, "notes": true} {
		if _, err := os.Stat(filepath.Join(root, d)); (err == nil) != kept {
			t.Errorf("%s: kept=%v, want %v", d, err == nil, kept)
		}
	}

	// Disabled: none.
	n.cfg.SessionCache.Disabled = true
	if d := n.sessionCache("codex", ulid.New()); d != "" {
		t.Fatalf("disabled, got %q", d)
	}
}

func closeSpools(n *Node) {
	for _, s := range n.sessionList() {
		s.mu.Lock()
		if s.spool != nil {
			_ = s.spool.Close()
			s.spool = nil
		}
		s.mu.Unlock()
	}
}
