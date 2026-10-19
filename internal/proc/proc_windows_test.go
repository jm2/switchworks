// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package proc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

const envConsoleProbe = "SWK_PROC_CONSOLE_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(envConsoleProbe) == "1" {
		consoleProbe()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// consoleProbe prints whether this process's console window is visible and
// the processes attached to its console.
func consoleProbe() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	visible, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("IsWindowVisible").Call(hwnd)
	pids := make([]uint32, 64)
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	fmt.Println("visible", visible)
	for _, pid := range pids[:min(int(n), len(pids))] {
		fmt.Println("pid", pid)
	}
}

// A harness gets a console of its own with no window: not the node's, and
// not a visible new one, which Windows Terminal would show as a tab whose
// closing ends the harness.
func TestChildConsoleHidden(t *testing.T) {
	p, err := Start(os.Args[0], nil, Options{Env: Environ(map[string]string{envConsoleProbe: "1"})})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout)
	if err := p.WaitContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "visible ") {
		t.Fatalf("probe printed %q", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch n, _ := strconv.Atoi(v); {
		case k == "visible" && n != 0:
			t.Error("the child's console window is visible")
		case k == "pid" && n == os.Getpid():
			t.Error("the child shares the test's console")
		}
	}
}

// A junction (which any user can make with mklink /J) must not lead a
// confined session out of its root.
func TestCheckWithinJunction(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	j := filepath.Join(root, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if err := CheckWithin(root, j); err == nil {
		t.Fatal("a junction out of the root was accepted")
	}
	if err := CheckWithin(root, root); err != nil {
		t.Fatal(err)
	}
}
