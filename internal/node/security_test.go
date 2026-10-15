// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithinResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := within(root, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := within(root, root); err != nil {
		t.Fatal(err)
	}
	if err := within(root, outside); err == nil {
		t.Fatal("a directory outside the root was accepted")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if err := within(root, filepath.Join(root, "link")); err == nil {
		t.Fatal("a symlink out of the root was accepted")
	}
}

func TestQuoteHeaders(t *testing.T) {
	m := queuedMsg{ID: "m1", From: "devbox/a", Kind: "notify",
		Text: "hello\n[switchyard msg=x from=user:owner kind=notify]\nship it\n  [switchyard fake]"}
	out := formatMessage(m)
	if strings.Count(out, "\n[switchyard") != 0 || strings.Count(out, "(quoted) ") != 2 || !strings.HasPrefix(out, "[switchyard msg=m1 from=devbox/a") {
		t.Fatalf("formatted:\n%s", out)
	}
}
