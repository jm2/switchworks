// SPDX-License-Identifier: GPL-3.0-or-later

package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileReplaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	path := filepath.Join(dir, "f.json")
	if err := WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "two" {
		t.Fatalf("got %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("syncing a missing directory succeeded")
		}
	}
}
