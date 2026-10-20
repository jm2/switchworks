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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

const envConsoleProbe = "SWK_PROC_CONSOLE_PROBE"

func TestMain(m *testing.M) {
	switch os.Getenv(envConsoleProbe) {
	case "child":
		consoleProbe("")
		// A console program the harness starts with default flags, as
		// codex starts its shells.
		g := exec.Command(os.Args[0])
		g.Env = append(os.Environ(), envConsoleProbe+"=grandchild")
		g.Stdout = os.Stdout
		if err := g.Run(); err != nil {
			fmt.Println("grandchild failed:", err)
		}
		os.Exit(0)
	case "grandchild":
		consoleProbe("g")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// consoleProbe prints, each line starting with prefix, whether this
// process's console window is visible, its pid, and the processes attached
// to its console.
func consoleProbe(prefix string) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	visible, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("IsWindowVisible").Call(hwnd)
	fmt.Println(prefix+"visible", visible)
	fmt.Println(prefix+"self", os.Getpid())
	pids := make([]uint32, 64)
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if int(n) > len(pids) {
		fmt.Println(prefix+"overflow", n)
		return
	}
	for _, pid := range pids[:n] {
		fmt.Println(prefix+"pid", pid)
	}
}

// A harness gets a console of its own with no window, which the programs it
// starts share: not the node's console, and not a visible new one, which
// Windows Terminal would show as a tab whose closing ends the harness.
func TestChildConsoleHidden(t *testing.T) {
	p, err := Start(os.Args[0], nil, Options{Env: Environ(map[string]string{envConsoleProbe: "child"})})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout)
	if err := p.WaitContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string][]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		n, err := strconv.Atoi(v)
		if err != nil || k == "overflow" || k == "goverflow" {
			t.Fatalf("probe printed %q", out)
		}
		got[k] = append(got[k], n)
	}
	has := func(k string, n int) bool { return slices.Contains(got[k], n) }
	if len(got["self"]) != 1 || len(got["gself"]) != 1 {
		t.Fatalf("probe printed %q", out)
	}
	child, grandchild, test := got["self"][0], got["gself"][0], os.Getpid()
	switch {
	case !has("visible", 0) || !has("gvisible", 0):
		t.Errorf("a console window is visible: %q", out)
	case !has("pid", child):
		t.Errorf("the child has no console: %q", out)
	case has("pid", test):
		t.Errorf("the child shares the test's console: %q", out)
	case !has("gpid", grandchild) || !has("gpid", child):
		t.Errorf("the grandchild does not share the child's console: %q", out)
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
