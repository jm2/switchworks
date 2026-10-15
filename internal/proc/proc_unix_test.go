// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package proc

import (
	"io"
	"os/exec"
	"strings"
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
