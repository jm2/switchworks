// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jm2/switchworks/internal/gpurun"
	"github.com/jm2/switchworks/internal/proc"
)

// GPU runs (gpu_run). Codex's Windows sandbox runs commands on a private
// desktop where Direct3D cannot reach the display. The node runs in the
// owner's interactive session, so on request it runs a command there for
// the session's agent, confined (gpurun: an AppContainer that sees the
// session's working directory and not the owner's profile, in a job with
// UI limits and a time limit), and returns its output. Off unless node.json
// sets gpu_run.enabled.

// maxGPUSeconds caps a run's time limit: under the MCP tool timeout, so the
// harness waits for the result.
const maxGPUSeconds = MCPToolTimeout - 60

// gpuRunOS is the OS GPU runs are implemented on (a variable for tests).
var gpuRunOS = "windows"

// runGPU runs a plan (a variable for tests).
var runGPU = gpurun.Run

func (n *Node) gpuRunsOn() bool {
	return n.cfg.GPURun.Enabled && runtime.GOOS == gpuRunOS
}

func (n *Node) gpuMax() time.Duration {
	s := n.cfg.GPURun.MaxSeconds
	if s <= 0 || s > maxGPUSeconds {
		s = maxGPUSeconds
	}
	return time.Duration(s) * time.Second
}

func (n *Node) handleGPURun(w http.ResponseWriter, r *http.Request) {
	id, s, done := n.localSession(w, r)
	if s == nil {
		return
	}
	defer done()
	if !n.gpuRunsOn() {
		msg := "GPU runs are off on " + n.cfg.Host + " (gpu_run.enabled in node.json)"
		if runtime.GOOS != gpuRunOS {
			msg = gpurun.ErrUnsupported.Error()
		}
		writeLocal(w, 404, LocalResponse{Error: msg})
		return
	}
	body, ok := readLocalBody(w, r)
	if !ok {
		return
	}
	if _, busy := n.gpuRuns.LoadOrStore(id, struct{}{}); busy {
		writeLocal(w, 200, LocalResponse{Error: "this session already has a GPU run in progress"})
		return
	}
	defer n.gpuRuns.Delete(id)
	plan, err := n.gpuPlan(s, body)
	if err != nil {
		writeLocal(w, 200, LocalResponse{Error: err.Error()})
		return
	}
	s.mu.Lock()
	path := s.rec.Path
	s.mu.Unlock()
	n.logf("session %s: GPU run of %s in %s (time limit %v)", path, plan.Program, plan.Cwd, plan.Timeout)
	res, err := runGPU(r.Context(), plan)
	if err != nil {
		n.logf("session %s: GPU run: %v", path, err)
		writeLocal(w, 200, LocalResponse{Error: err.Error()})
		return
	}
	n.logf("session %s: GPU run ended: exit %d, timed out %v, %d ms", path, res.ExitCode, res.TimedOut, res.DurationMS)
	data, _ := json.Marshal(res)
	writeLocal(w, 200, LocalResponse{OK: true, Data: data})
}

// gpuPlan checks a GPU run request against the session and the node's
// policy: the workspace is the session's working directory (pinned, as for
// attachments), which must not hold the home directory or lie in a denied
// path; .git and the deny list's entries inside it are protected.
func (n *Node) gpuPlan(s *session, body []byte) (gpurun.Plan, error) {
	var req gpurun.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return gpurun.Plan{}, fmt.Errorf("bad arguments: %w", err)
	}
	if err := gpurun.Validate(req, n.gpuMax()); err != nil {
		return gpurun.Plan{}, err
	}
	s.mu.Lock()
	cwd, root, sandbox := s.rec.Spec.Cwd, s.rec.Spec.CwdRoot, s.rec.Spec.Sandbox
	s.mu.Unlock()
	if sandbox == "read-only" {
		return gpurun.Plan{}, fmt.Errorf("this session's sandbox is read-only; GPU runs write the workspace")
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	if err := proc.CheckPinned(root, cwd); err != nil {
		return gpurun.Plan{}, err
	}
	ws, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return gpurun.Plan{}, fmt.Errorf("the session's working directory: %w", err)
	}
	pol := n.attachPolicy()
	deny := append(append([]string{}, pol.Deny...), pol.Own...)
	// (A workspace holding the profiles folder holds home too.)
	system := gpurun.SystemDirs(os.Getenv, pol.Home)
	if err := gpurun.CheckWorkspace(ws, pol.Home, deny, system); err != nil {
		return gpurun.Plan{}, err
	}
	runCwd, err := gpurun.ResolveCwd(ws, req.Cwd)
	if err != nil {
		return gpurun.Plan{}, err
	}
	prog, err := gpurun.FindProgram(req.Argv[0], runCwd)
	if err != nil {
		return gpurun.Plan{}, err
	}
	// Checked by its real path (links resolved, long names), as Windows
	// will open it.
	if prog, err = filepath.EvalSymlinks(prog); err != nil {
		return gpurun.Plan{}, fmt.Errorf("%s: %w", req.Argv[0], err)
	}
	if err := gpurun.CheckProgramDir(prog, ws, gpurun.SystemDirs(os.Getenv, "")); err != nil {
		return gpurun.Plan{}, err
	}
	ro, hidden := gpurun.Protected(ws, deny)
	for _, h := range hidden {
		if gpurun.Within(h, prog) {
			return gpurun.Plan{}, fmt.Errorf("%s is on the deny list", prog)
		}
	}
	return gpurun.Plan{Workspace: ws, Cwd: runCwd, Program: prog, Argv: req.Argv, Env: req.Env,
		Timeout: req.Timeout(), Network: n.cfg.GPURun.Network, Desktop: gpurun.DefaultDesktop,
		ReadOnly: ro, Hidden: hidden}, nil
}
