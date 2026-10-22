// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/model"
)

func TestSniffText(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"fix.patch", "From abc\n", "text/x-patch"},
		{"FIX.PATCH", "x", "text/x-patch"},
		{"a.diff", "--- a\n+++ b\n", "text/x-diff"},
		{"notes.md", "# hi", "text/markdown"},
		{"notes.markdown", "# hi", "text/markdown"},
		{"x.json", `{"a": 1}`, "application/json"},
		{"bad.json", `{"a": `, "text/plain"},
		{"build.log", "ok\r\n", "text/plain"},
		{"noext", "ünïcode", "text/plain"},
		{"shell.sh", "#!/bin/sh\n", "text/plain"},
		{"nul.txt", "a\x00b", ""},
		{"latin1.txt", "caf\xe9", ""},
		{"x.png", string(pngBytes(t, 2)), ""},
	}
	for _, c := range cases {
		if got := SniffText(c.name, []byte(c.data)); got != c.want {
			t.Errorf("SniffText(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	for _, c := range cases {
		if c.want != "" && !contains(AllowedFiles, c.want) {
			t.Errorf("%s is not in AllowedFiles", c.want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestNormalizeFiles(t *testing.T) {
	out, err := NormalizeFiles([]model.Attachment{{Name: "../../etc/fix.patch", Data: []byte("diff\n")}})
	if err != nil || out[0].Name != "fix.patch" || out[0].MIME != "text/x-patch" || out[0].Size != 5 || out[0].SHA256 != SHA256([]byte("diff\n")) {
		t.Fatalf("normalize: %+v %v", out, err)
	}
	if out, _ := NormalizeFiles([]model.Attachment{{Name: "", Data: []byte("x")}}); out[0].Name != "file-1.txt" {
		t.Fatalf("unnamed: %q", out[0].Name)
	}
	mib := bytes.Repeat([]byte("a"), MaxFileBytes)
	refused := func(why string, atts ...model.Attachment) {
		t.Helper()
		if _, err := NormalizeFiles(atts); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", why, err)
		}
	}
	refused("empty", model.Attachment{Name: "a.txt"})
	refused("not text", model.Attachment{Name: "a.txt", Data: []byte{0xff, 0xfe}})
	refused("limit is", model.Attachment{Name: "a.txt", Data: append(mib, 'b')})
	refused("at most 4", model.Attachment{Data: []byte("1")}, model.Attachment{Data: []byte("2")}, model.Attachment{Data: []byte("3")},
		model.Attachment{Data: []byte("4")}, model.Attachment{Data: []byte("5")})
	refused("per message", model.Attachment{Data: mib}, model.Attachment{Data: mib}, model.Attachment{Data: []byte("x")})
	refused("declared", model.Attachment{Name: "a.txt", MIME: "image/png", Data: []byte("x")})
	refused("sha256", model.Attachment{Name: "a.txt", SHA256: SHA256([]byte("y")), Data: []byte("x")})
	if _, err := NormalizeFiles([]model.Attachment{{Data: mib}, {Data: mib}}); err != nil {
		t.Fatalf("two files of 1 MiB: %v", err)
	}
}

func TestDiskNames(t *testing.T) {
	atts := []model.Attachment{{Name: "fix.patch"}, {Name: "fix.patch"}, {Name: "CON.txt"}, {Name: "nul"}, {Name: "com1.log"},
		{Name: "trail. "}, {Name: "..."}, {Name: "Fix.patch"}}
	got := strings.Join(DiskNames(atts), ",")
	want := "fix.patch,fix-2.patch,_CON.txt,_nul,_com1.log,trail,file-7.txt,Fix-3.patch"
	if got != want {
		t.Fatalf("DiskNames = %s, want %s", got, want)
	}
}

// ReadFileWithin applies ReadWithin's path checks to text files.
func TestReadFileWithin(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	nodeDir := filepath.Join(cwd, ".config", "swk", "node")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{cwd, filepath.Join(cwd, "out"), nodeDir, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	patch := []byte("From 0123\nSubject: [PATCH] x\n\ndiff --git a/x b/x\n")
	write := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cwd, "out", "fix.patch"), patch)
	write(filepath.Join(outside, "secret.txt"), []byte("token"))
	write(filepath.Join(nodeDir, "node.key"), []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"))
	write(filepath.Join(cwd, "shot.png"), pngBytes(t, 2))
	write(filepath.Join(cwd, "big.log"), bytes.Repeat([]byte("x"), MaxFileBytes+1))

	a, err := ReadFileWithin(cwd, "out/fix.patch", []string{nodeDir})
	if err != nil || !bytes.Equal(a.Data, patch) || a.Name != "fix.patch" || a.MIME != "text/x-patch" {
		t.Fatalf("ReadFileWithin: %+v %v", a, err)
	}
	if _, err := ReadFileWithin(cwd, filepath.Join(cwd, "out", "fix.patch"), nil); err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	refused := func(name, why string) {
		t.Helper()
		a, err := ReadFileWithin(cwd, name, []string{nodeDir})
		if err == nil {
			t.Fatalf("ReadFileWithin(%q) accepted (%d bytes)", name, len(a.Data))
		}
		if !strings.Contains(err.Error(), why) {
			t.Fatalf("ReadFileWithin(%q): %v (want %q)", name, err, why)
		}
	}
	refused("../outside/secret.txt", "files must be files under it")
	refused(filepath.Join(outside, "secret.txt"), "outside")
	refused(filepath.Join(nodeDir, "node.key"), "Switchworks's own directory")
	refused(".config/swk/node/node.key", "Switchworks's own directory")
	refused("shot.png", "not a text file")
	refused("big.log", "limit is")
	refused("out", "not a regular file")
	refused("missing.txt", "no such file")
	refused("", "invalid file path")

	if runtime.GOOS == "windows" {
		return // symlinks need privileges there
	}
	if err := os.Symlink(filepath.Join(cwd, "out", "fix.patch"), filepath.Join(cwd, "link.patch")); err != nil {
		t.Fatal(err)
	}
	refused("link.patch", "symlink")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(cwd, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	refused("escape.txt", "outside")
	if err := os.Symlink(filepath.Join(cwd, "out"), filepath.Join(cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	refused("alias/fix.patch", "symlink")
	if err := os.Symlink(nodeDir, filepath.Join(cwd, "nd")); err != nil {
		t.Fatal(err)
	}
	refused("nd/node.key", "Switchworks's own directory")
	// A hard link to the node's key.
	if err := os.Link(filepath.Join(nodeDir, "node.key"), filepath.Join(cwd, "key.txt")); err == nil {
		refused("key.txt", "hard links")
	}
	if err := mkfifo(filepath.Join(cwd, "pipe.log")); err == nil {
		refused("pipe.log", "not a regular file")
	}
}

func TestReadTextFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	_ = os.WriteFile(p, []byte("# notes\n"), 0o600)
	a, err := ReadTextFile(p)
	if err != nil || a.Name != "notes.md" || a.MIME != "text/markdown" {
		t.Fatalf("ReadTextFile: %+v %v", a, err)
	}
	bin := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(bin, []byte{0, 1, 2}, 0o600)
	if _, err := ReadTextFile(bin); err == nil {
		t.Fatal("binary accepted")
	}
	if _, err := ReadTextFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
}
