// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package proc

import (
	"os/exec"
	"path/filepath"
	"testing"
)

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
