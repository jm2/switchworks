// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/gpurun"
)

// gpuNode is a node with its local API up and one session working in a new
// workspace (ws, with a .git, a denied keys directory and a tool).
func gpuNode(t *testing.T, enabled bool) (n *Node, s *session, ws string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	ws = filepath.Join(home, "src", "game")
	for _, d := range []string{".git", "keys", "sub"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, _ = filepath.EvalSymlinks(ws)
	for _, f := range []string{"tool", "tool.exe", "keys/tool", "keys/tool.exe"} {
		os.WriteFile(filepath.Join(ws, filepath.FromSlash(f)), []byte("#!/bin/sh\n"), 0o755)
	}
	n = &Node{dir: filepath.Join(home, "swk-node"), logf: t.Logf, sessions: map[string]*session{}}
	n.cfg.Host = "h"
	n.cfg.GPURun.Enabled = enabled
	n.cfg.AttachDeny = []string{filepath.Join(ws, "keys")}
	if err := n.startLocal(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.local.close)
	s = &session{n: n, rec: record{ID: "s1", Path: "h/s1", Spec: adapter.StartSpec{Cwd: ws, Sandbox: "workspace-write"}}}
	n.sessions["s1"] = s
	old, oldRun := gpuRunOS, runGPU
	gpuRunOS = runtime.GOOS
	t.Cleanup(func() { gpuRunOS, runGPU = old, oldRun })
	return n, s, ws
}

func gpuPost(t *testing.T, n *Node, token, body string) (int, LocalResponse) {
	t.Helper()
	req, _ := http.NewRequest("POST", n.local.url+"/v1/gpu/run", strings.NewReader(body))
	req.Header.Set("X-Swk-Session", "s1")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var lr LocalResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, lr
}

func TestGPURunOff(t *testing.T) {
	n, _, _ := gpuNode(t, false)
	if code, lr := gpuPost(t, n, n.local.token("s1"), `{"argv":["./tool"]}`); code != 404 || !strings.Contains(lr.Error, "gpu_run.enabled") {
		t.Errorf("%d %+v", code, lr)
	}
	if _, ok := n.mcpFor("s1", "h/s1").Env[EnvGPURun]; ok {
		t.Error("gpu_run offered while off")
	}
	gpuRunOS = "plan9"
	n.cfg.GPURun.Enabled = true
	if code, lr := gpuPost(t, n, n.local.token("s1"), `{"argv":["./tool"]}`); code != 404 || !strings.Contains(lr.Error, "only implemented on Windows") {
		t.Errorf("%d %+v", code, lr)
	}
}

func TestGPURunPlan(t *testing.T) {
	n, s, ws := gpuNode(t, true)
	if n.mcpFor("s1", "h/s1").Env[EnvGPURun] != "1" {
		t.Error("gpu_run not offered")
	}
	var got gpurun.Plan
	runGPU = func(ctx context.Context, p gpurun.Plan) (gpurun.Result, error) {
		got = p
		return gpurun.Result{ExitCode: 3, Stdout: "frame 1\n", DurationMS: 5}, nil
	}
	tok := n.local.token("s1")
	if code, _ := gpuPost(t, n, "bad", `{"argv":["./tool"]}`); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	code, lr := gpuPost(t, n, tok, `{"argv":["../tool","--frames","1"],"cwd":"sub","env":{"SDL_VIDEODRIVER":"windows"},"timeout_seconds":30}`)
	if code != 200 || !lr.OK {
		t.Fatalf("%d %+v", code, lr)
	}
	var res gpurun.Result
	if err := json.Unmarshal(lr.Data, &res); err != nil || res.ExitCode != 3 || res.Stdout != "frame 1\n" {
		t.Errorf("result %s %v", lr.Data, err)
	}
	if got.Workspace != ws || got.Cwd != filepath.Join(ws, "sub") || filepath.Dir(got.Program) != ws ||
		!strings.HasPrefix(filepath.Base(got.Program), "tool") || got.Timeout != 30*time.Second ||
		got.Desktop != gpurun.DefaultDesktop || got.Network || got.Env["SDL_VIDEODRIVER"] != "windows" ||
		len(got.Argv) != 3 || got.Argv[0] != "../tool" {
		t.Errorf("plan %+v", got)
	}
	if len(got.ReadOnly) != 1 || got.ReadOnly[0] != filepath.Join(ws, ".git") || len(got.Hidden) != 1 || got.Hidden[0] != filepath.Join(ws, "keys") {
		t.Errorf("protected %q %q", got.ReadOnly, got.Hidden)
	}

	for body, want := range map[string]string{
		`{"argv":[]}`:                                 "argv",
		`{"argv":["./tool"],"shell":true}`:            "unknown field",
		`{"argv":["./tool"],"cwd":".."}`:              "outside",
		`{"argv":["keys/tool"]}`:                      "deny list",
		`{"argv":["./missing"]}`:                      "no such program",
		`{"argv":["./tool"],"timeout_seconds":99999}`: "at most",
	} {
		if _, lr := gpuPost(t, n, tok, body); lr.OK || !strings.Contains(lr.Error, want) {
			t.Errorf("%s: %+v, want %q", body, lr, want)
		}
	}

	s.rec.Spec.Sandbox = "read-only"
	if _, lr := gpuPost(t, n, tok, `{"argv":["./tool"]}`); !strings.Contains(lr.Error, "read-only") {
		t.Errorf("read-only session: %+v", lr)
	}
	s.rec.Spec.Sandbox = "workspace-write"
	home, _ := os.UserHomeDir()
	s.rec.Spec.Cwd = home
	if _, lr := gpuPost(t, n, tok, `{"argv":["./tool"]}`); !strings.Contains(lr.Error, "home directory") {
		t.Errorf("home as workspace: %+v", lr)
	}
	s.rec.Spec.Cwd = ws

	// One run per session at a time.
	release := make(chan struct{})
	started := make(chan struct{})
	runGPU = func(ctx context.Context, p gpurun.Plan) (gpurun.Result, error) {
		close(started)
		<-release
		return gpurun.Result{}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); gpuPost(t, n, tok, `{"argv":["./tool"]}`) }()
	<-started
	if _, lr := gpuPost(t, n, tok, `{"argv":["./tool"]}`); !strings.Contains(lr.Error, "already has a GPU run") {
		t.Errorf("second run: %+v", lr)
	}
	close(release)
	<-done
	n.cfg.GPURun.Network = true
	runGPU = func(ctx context.Context, p gpurun.Plan) (gpurun.Result, error) { got = p; return gpurun.Result{}, nil }
	if _, lr := gpuPost(t, n, tok, `{"argv":["./tool"]}`); !lr.OK || !got.Network || got.Timeout != gpurun.DefaultTimeout || got.Cwd != ws {
		t.Errorf("network run: %+v %+v", lr, got)
	}
}
