// SPDX-License-Identifier: GPL-3.0-or-later

package winsys

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemDLL(t *testing.T) {
	dir, ok := systemDir()
	if !ok || !filepath.IsAbs(dir) || !strings.EqualFold(filepath.Base(dir), "System32") {
		t.Fatalf("system directory %q %v", dir, ok)
	}
	d := DLL("user32.dll")
	if !strings.EqualFold(d.Name, filepath.Join(dir, "user32.dll")) {
		t.Fatalf("DLL path %q", d.Name)
	}
	if err := d.Load(); err != nil {
		t.Fatal(err)
	}
}
