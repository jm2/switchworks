// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package proc

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Output written just before a fast exit is not lost, and a descendant that
// escapes the process group and keeps stdout open does not keep the reader
// blocked for long after the process exits.
func TestStdoutAfterExit(t *testing.T) {
	for i := 0; i < 20; i++ {
		p, err := Start("sh", []string{"-c", "echo last-line"}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p.Stdout)
		<-p.Done()
		if !strings.Contains(string(b), "last-line") {
			t.Fatalf("run %d: lost output %q", i, b)
		}
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	p, err := Start("sh", []string{"-c", "setsid sleep 30 & echo hi; exit 0"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eof := make(chan string)
	go func() { b, _ := io.ReadAll(p.Stdout); eof <- string(b) }()
	<-p.Done()
	select {
	case out := <-eof:
		if !strings.Contains(out, "hi") {
			t.Fatalf("output %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stdout reader still blocked 10s after the process exited")
	}
}

// A leader that exits on stdin EOF does not leave its children running.
func TestStopEndsChildrenOfExitedLeader(t *testing.T) {
	p, err := Start("sh", []string{"-c", "sleep 300 & echo $!; read x; exit 0"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(p.Stdout, &pid); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Stop(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("Stop took %v", d)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("child %d still running after Stop (kill 0: %v)", pid, err)
	}
}
