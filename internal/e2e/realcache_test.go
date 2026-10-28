// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/codex/codextest"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/node"
)

// TestRealCodexSessionCache runs real `codex app-server` sessions in
// workspace-write, under the legacy sandbox and under a permission
// profile: each can write its own cache directory through CCACHE_DIR and
// XDG_CACHE_HOME, not another session's, and its commands see
// codex.session_env (a variable, and a PATH directory ahead of the rest).
//
// Gated like TestRealCodexFleetAsk: SWK_CODEX_IT=1 go test ./internal/e2e/
func TestRealCodexSessionCache(t *testing.T) {
	if os.Getenv("SWK_CODEX_IT") != "1" {
		t.Skip("set SWK_CODEX_IT=1 to run against the real codex binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the probe commands are POSIX sh")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}
	mock, err := codextest.StartMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	for _, profile := range []bool{false, true} {
		name := "legacy"
		if profile {
			name = "profile"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			coordDir := t.TempDir()
			co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
			defer co.stop()
			c := apiClient(t, coordDir)
			tools := t.TempDir()
			if err := os.WriteFile(filepath.Join(tools, "swk-probe-tool"), []byte("#!/bin/sh\necho probe\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			var cacheRoot string
			nd := startRealNode(t, c, "devbox", bin, mock, func(dir string, cfg *node.Config) {
				cacheRoot = cfg.SessionCache.Dir
				// Every session may write /tmp and $TMPDIR, so for the
				// isolation check the cache root must be under neither.
				if strings.HasPrefix(cacheRoot, "/tmp/") {
					t.Skip("set TMPDIR to a directory outside /tmp")
				}
				cfg.Codex.SessionEnv = node.SessionEnv{Set: map[string]string{"SWK_PROBE": "from-node-json", "TMPDIR": t.TempDir()},
					PathPrepend: []string{tools}}
				if profile {
					f, err := os.OpenFile(filepath.Join(cfg.Codex.Home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					// default_permissions must come before any table, so
					// the profile is named with -c instead.
					_, err = f.WriteString("\n[permissions.swk]\nextends = \":workspace\"\n")
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
					cfg.Codex.Config = map[string]json.RawMessage{"default_permissions": json.RawMessage(`"swk"`)}
					cfg.Codex.Permissions = "swk"
				}
			})
			defer nd.stop(t)
			waitOnline(t, c, "devbox")

			spawn := func(name string) (string, *watcher) {
				s, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: name, Cwd: t.TempDir(), ApprovalPolicy: "never", Sandbox: "workspace-write"})
				if err != nil {
					t.Fatal(err)
				}
				w := watch(c, s.ID)
				waitStatus(t, c, s.ID, model.StatusIdle)
				return s.ID, w
			}
			run := func(w *watcher, id, cmd string) {
				t.Helper()
				n := w.len()
				if _, err := c.Send(ctx, id, "RUN: "+cmd, "queue", ""); err != nil {
					t.Fatal(err)
				}
				w.waitFor(t, "command done", n, turnEnded("completed"))
			}
			a, wa := spawn("a")
			defer wa.cancel()
			b, wb := spawn("b")
			defer wb.cancel()
			realRoot, _ := filepath.EvalSymlinks(cacheRoot)
			dirA, dirB := filepath.Join(realRoot, a), filepath.Join(realRoot, b)

			run(wa, a, `printf '%s\n' "$CCACHE_DIR" "$XDG_CACHE_HOME" "$SWK_PROBE" "$(swk-probe-tool)" > "$CCACHE_DIR/probe"`)
			got, err := os.ReadFile(filepath.Join(dirA, "probe"))
			if err != nil {
				t.Fatalf("session a could not write its cache: %v", err)
			}
			if want := dirA + "\n" + dirA + "\nfrom-node-json\nprobe\n"; string(got) != want {
				t.Fatalf("probe wrote %q, want %q", got, want)
			}
			run(wb, b, `touch "`+filepath.Join(dirA, "intruder")+`"; touch "$CCACHE_DIR/mine"`)
			if _, err := os.Stat(filepath.Join(dirA, "intruder")); !os.IsNotExist(err) {
				t.Fatalf("session b wrote into session a's cache: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dirB, "mine")); err != nil {
				t.Fatalf("session b could not write its own cache: %v", err)
			}
			if err := c.Stop(ctx, a); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, c, a, model.StatusStopped)
			if _, err := os.Stat(dirA); !os.IsNotExist(err) {
				t.Fatalf("cache kept after stop: %v", err)
			}
		})
	}
}
