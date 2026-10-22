// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package proc

import (
	"io"
	"os/exec"
	"testing"
	"time"
)

// A descendant that leaves the group with the stderr pipe open does not
// keep Kill waiting.
func TestKillWithStderrHeldOutsideGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	p, err := Start("sh", []string{"-c", "setsid sleep 30 & cat >/dev/null"}, Options{Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	_ = p.Kill()
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("Kill took %v", d)
	}
}
