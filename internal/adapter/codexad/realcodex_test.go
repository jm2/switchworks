package codexad

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/codex/codextest"
	"github.com/jm2/switchyard/internal/model"
)

// TestRealCodex drives the real `codex app-server` against a local mock
// model provider: a full turn with streaming and usage, an approval round
// trip for a shell command, and resume across app-server processes.
//
// Gated because it needs the codex binary: SWYD_CODEX_IT=1 go test ./...
// Set SWYD_CODEX_RECORD=<file> to save the raw protocol traffic.
func TestRealCodex(t *testing.T) {
	if os.Getenv("SWYD_CODEX_IT") != "1" {
		t.Skip("set SWYD_CODEX_IT=1 to run against the real codex binary")
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
	home := filepath.Join(t.TempDir(), "codex-home")
	env, err := mock.WriteCodexHome(home)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var tapMu sync.Mutex
	var tapBuf bytes.Buffer
	cfg := Config{Bin: bin, Env: env, Stderr: &stderr, Logf: t.Logf,
		Tap: func(dir string, line []byte) {
			tapMu.Lock()
			tapBuf.WriteString(dir + " " + string(line) + "\n")
			tapMu.Unlock()
		}}
	defer func() {
		if path := os.Getenv("SWYD_CODEX_RECORD"); path != "" {
			tapMu.Lock()
			_ = os.WriteFile(path, tapBuf.Bytes(), 0o644)
			tapMu.Unlock()
		}
		if t.Failed() {
			t.Logf("app-server stderr:\n%s", stderr.String())
		}
	}()

	ctx := context.Background()
	work := t.TempDir()
	a := New(cfg)
	defer a.Close()

	// 1. A plain turn.
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Name: "test/plain", Cwd: work, ApprovalPolicy: "never", Sandbox: "workspace-write"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := a.Info(); !ok || info.CodexHome != home {
		t.Fatalf("codex home %q, want %q", info.CodexHome, home)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "hello"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "delta", kind(model.KindItemDelta))
	i = col.waitFor(t, i, "reply", completedText("mock: hello"))
	j := col.waitFor(t, i, "usage", kind(model.KindUsage))
	u := col.snapshot()[j].Payload.(model.UsageEvent)
	if u.Total.InputTokens != 120 || u.Total.CachedInputTokens != 40 || u.Total.OutputTokens != 7 {
		t.Fatalf("usage %+v", u.Total)
	}
	col.waitFor(t, i, "turn completed", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == "completed"
	})
	ref := h.Ref()

	// 2. An approval round trip.
	col2 := &collector{}
	h2, err := a.Start(ctx, adapter.StartSpec{Name: "test/approve", Cwd: work, ApprovalPolicy: "untrusted", Sandbox: "workspace-write"}, col2.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Deliver(ctx, adapter.Input{Text: "RUN: touch approved.txt"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	k := col2.waitFor(t, 0, "approval.requested", kind(model.KindApprovalRequested))
	req := col2.snapshot()[k].Payload.(model.ApprovalRequested)
	if req.Kind != "command" || !strings.Contains(req.Command, "touch approved.txt") {
		t.Fatalf("approval request %+v", req)
	}
	t.Logf("codex offered decisions: %v", req.Decisions)
	if err := h2.Resolve(ctx, req.ApprovalID, "accept"); err != nil {
		t.Fatal(err)
	}
	col2.waitFor(t, k, "final message", completedText("Command finished."))
	col2.waitFor(t, k, "turn completed", kind(model.KindTurnCompleted))
	if _, err := os.Stat(filepath.Join(work, "approved.txt")); err != nil {
		t.Logf("approved command did not create the file (sandbox may be unavailable here): %v", err)
	}

	// 3. Resume the first thread on a fresh app-server process.
	a.Close()
	a2 := New(cfg)
	defer a2.Close()
	col3 := &collector{}
	h3, err := a2.Resume(ctx, ref, adapter.StartSpec{Cwd: work, ApprovalPolicy: "never", Sandbox: "workspace-write"}, col3.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h3.Deliver(ctx, adapter.Input{Text: "again"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col3.waitFor(t, 0, "reply after resume", completedText("mock: again"))
}
