// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package gpurun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var procDeleteAppContainerProfile = userenv.NewProc("DeleteAppContainerProfile")

// plan is a run of cmd.exe /d /c script in a new workspace, which it
// returns, with a .git and a hidden file in it.
func plan(t *testing.T, script string) (Plan, string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		n, _ := syscall.UTF16PtrFromString(ContainerName(ws))
		procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(n)))
	})
	for _, d := range []string{".git", "sub"} {
		if err := os.Mkdir(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("hunter2\n"), 0o644)
	cmd, err := FindProgram("cmd.exe", ws)
	if err != nil {
		t.Fatal(err)
	}
	ro, hidden := Protected(ws, []string{filepath.Join(ws, "secret.txt")})
	return Plan{Workspace: ws, Cwd: ws, Program: cmd, Argv: []string{"cmd.exe", "/d", "/c", script},
		Env: map[string]string{"GREETING": "hi"}, Timeout: time.Minute, ReadOnly: ro, Hidden: hidden}, ws
}

// TestRunConfined runs cmd.exe in the AppContainer (on the node's own
// desktop: CI has no interactive one) and checks what it can reach.
func TestRunConfined(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("owner's\n"), 0o600)
	t.Setenv("SWK_TEST_SECRET", "s3cr3t")
	script := strings.Join([]string{
		"echo %GREETING%",
		"echo err 1>&2",
		"echo x> out.txt",
		"(echo y>> .git\\config && echo GITWRITE)",
		"(ren .git moved && echo GITRENAME)",
		"(type secret.txt && echo HIDDENREAD)",
		"(type " + outside + " && echo OUTREAD)",
		"(echo z> " + filepath.Dir(outside) + "\\planted.txt && echo OUTWRITE)",
		"(type .git\\config && echo GITREAD)",
		"(icacls .git\\config /grant *S-1-15-2-1:F && echo GITACL)",
		"set",
		"exit /b 7",
	}, " & ")
	p, ws := plan(t, script)
	res, err := Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	if res.ExitCode != 7 || res.TimedOut {
		t.Errorf("exit %d, timed out %v", res.ExitCode, res.TimedOut)
	}
	if !strings.Contains(res.Stdout, "hi") || !strings.Contains(res.Stderr, "err") {
		t.Error("output not captured")
	}
	if b, err := os.ReadFile(filepath.Join(ws, "out.txt")); err != nil || !strings.HasPrefix(string(b), "x") {
		t.Errorf("workspace not writable: %q %v", b, err)
	}
	for _, s := range []string{"GITWRITE", "GITRENAME", "HIDDENREAD", "hunter2", "OUTREAD", "owner's", "OUTWRITE", "s3cr3t", "GITACL"} {
		if strings.Contains(res.Stdout, s) {
			t.Errorf("the run could: %s", s)
		}
	}
	if !strings.Contains(res.Stdout, "GITREAD") {
		t.Error(".git not readable")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(b) != "[core]\n" {
		t.Errorf(".git/config changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "planted.txt")); err == nil {
		t.Error("wrote outside the workspace")
	}
}

func TestRunTimeout(t *testing.T) {
	p, _ := plan(t, "for /l %i in (0,0,1) do @rem")
	p.Timeout = 2 * time.Second
	start := time.Now()
	res, err := Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 30*time.Second {
		t.Errorf("timed out %v after %v", res.TimedOut, time.Since(start))
	}
}

func TestRunCancel(t *testing.T) {
	p, _ := plan(t, "for /l %i in (0,0,1) do @rem")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Run(ctx, p); err == nil {
		t.Error("a cancelled run returned no error")
	}
}

func TestRunRefusesLinkedGit(t *testing.T) {
	p, ws := plan(t, "exit /b 0")
	other := filepath.Join(ws, "sub")
	os.RemoveAll(filepath.Join(ws, ".git"))
	// A junction needs no privilege.
	if err := mkJunction(filepath.Join(ws, ".git"), other); err != nil {
		t.Skip("cannot make a junction:", err)
	}
	p.ReadOnly, p.Hidden = Protected(ws, nil)
	if _, err := Run(context.Background(), p); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("a .git junction was accepted: %v", err)
	}
}

func mkJunction(link, target string) error {
	return exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).Run()
}
