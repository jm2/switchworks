// SPDX-License-Identifier: GPL-3.0-or-later

package proc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jm2/switchworks/internal/proc"
)

// A recorded cwd swapped for a symlink fails CheckPinned, even when the
// link stays inside the root (where CheckWithin alone would pass).
func TestCheckPinned(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub, other := filepath.Join(root, "sub"), filepath.Join(root, "other")
	for _, d := range []string{sub, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := proc.CheckPinned(root, sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sub, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, sub); err != nil {
		t.Skip("no symlinks:", err)
	}
	if err := proc.CheckWithin(root, sub); err != nil {
		t.Fatalf("CheckWithin: %v", err)
	}
	if err := proc.CheckPinned(root, sub); err == nil {
		t.Fatal("a cwd swapped for a symlink passed")
	}
	if err := proc.CheckPinned("", sub); err != nil {
		t.Fatalf("no root: %v", err)
	}
}
