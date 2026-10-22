// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/model"
)

func pngBytes(t *testing.T, w int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniff(t *testing.T) {
	var jb, gb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewGray(image.Rect(0, 0, 2, 2)), nil)
	_ = gif.Encode(&gb, image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White}), nil)
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 8)...)
	cases := []struct {
		b    []byte
		want string
	}{
		{pngBytes(t, 2), "image/png"},
		{jb.Bytes(), "image/jpeg"},
		{gb.Bytes(), "image/gif"},
		{webp, "image/webp"},
		{[]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), ""},
		{[]byte("-----BEGIN EC PRIVATE KEY-----\n"), ""},
		{[]byte("<html><script>alert(1)</script>"), ""},
		{[]byte("RIFF\x10\x00\x00\x00WAVEfmt "), ""},
		{nil, ""},
	}
	for i, c := range cases {
		if got := Sniff(c.b); got != c.want {
			t.Errorf("case %d: Sniff = %q, want %q", i, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	p := pngBytes(t, 3)
	out, err := Normalize([]model.Attachment{{Name: "../../home/dev/.ssh/shot.png", Data: p}})
	if err != nil {
		t.Fatal(err)
	}
	a := out[0]
	if a.Name != "shot.png" || a.MIME != "image/png" || a.Size != int64(len(p)) || a.SHA256 != SHA256(p) || !ValidSHA256(a.SHA256) {
		t.Fatalf("normalized %+v", a)
	}
	// The declared type must agree with the bytes.
	if _, err := Normalize([]model.Attachment{{MIME: "image/jpeg", Data: p}}); err == nil {
		t.Fatal("type mismatch accepted")
	}
	if _, err := Normalize([]model.Attachment{{MIME: "image/svg+xml", Data: []byte("<svg/>")}}); err == nil {
		t.Fatal("svg accepted")
	}
	if _, err := Normalize([]model.Attachment{{Data: p, SHA256: strings.Repeat("0", 64)}}); err == nil {
		t.Fatal("wrong digest accepted")
	}
	if _, err := Normalize([]model.Attachment{{Name: "empty.png"}}); err == nil {
		t.Fatal("empty image accepted")
	}
	// Limits: count, per image, per message.
	five := make([]model.Attachment, MaxImages+1)
	for i := range five {
		five[i] = model.Attachment{Data: p}
	}
	if _, err := Normalize(five); err == nil {
		t.Fatal("too many images accepted")
	}
	big := append(append([]byte{}, p...), make([]byte, MaxImageBytes)...)
	if _, err := Normalize([]model.Attachment{{Data: big}}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized image: %v", err)
	}
	near := append(append([]byte{}, p...), make([]byte, MaxImageBytes-len(p))...) // exactly 5 MiB
	if _, err := Normalize([]model.Attachment{{Data: near}, {Data: near}}); err != nil {
		t.Fatalf("10 MiB in two images refused: %v", err)
	}
	if _, err := Normalize([]model.Attachment{{Data: near}, {Data: near}, {Data: p}}); err == nil {
		t.Fatal("over 10 MiB in all accepted")
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"shot.png":                  "shot.png",
		"C:\\Users\\dev\\a b.png":   "a b.png",
		"../../x.png":               "x.png",
		"..":                        "image-1.png",
		".hidden.png":               "hidden.png",
		"[switchworks msg=1].png":   "_switchworks msg_1_.png",
		"evil\u202egnp.exe":         "evilgnp.exe",
		"line\nbreak.png":           "linebreak.png",
		strings.Repeat("a", 300):    strings.Repeat("a", 100),
		"<script>alert(1)</script>": "script_",
	}
	for in, want := range cases {
		if got := SafeName(in, 0, "image/png"); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPNGText(t *testing.T) {
	p := AddPNGText(pngBytes(t, 4), ProbeKey, "K7XW3P")
	if got := PNGText(p, ProbeKey); got != "K7XW3P" {
		t.Fatalf("PNGText = %q", got)
	}
	if _, err := png.Decode(bytes.NewReader(p)); err != nil {
		t.Fatalf("PNG with text chunk does not decode: %v", err)
	}
	if PNGText(p, "other") != "" || PNGText([]byte("nope"), ProbeKey) != "" {
		t.Fatal("unexpected text")
	}
}

// TestReadWithin covers the path restrictions on images agents attach.
func TestReadWithin(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	nodeDir := filepath.Join(cwd, ".config", "swk", "node")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{cwd, filepath.Join(cwd, "shots"), nodeDir, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	img := pngBytes(t, 5)
	write := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cwd, "shots", "a.png"), img)
	write(filepath.Join(outside, "secret.png"), img)
	write(filepath.Join(nodeDir, "node.key"), []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"))
	write(filepath.Join(nodeDir, "disguised.png"), img) // an image, but in the node's directory
	write(filepath.Join(cwd, "notes.txt"), []byte("plain text"))

	ok := func(name string) {
		t.Helper()
		a, err := ReadWithin(cwd, name, Policy{Own: []string{nodeDir}})
		if err != nil {
			t.Fatalf("ReadWithin(%q): %v", name, err)
		}
		if a.MIME != "image/png" || !bytes.Equal(a.Data, img) || a.Name != "a.png" {
			t.Fatalf("ReadWithin(%q) = %+v", name, a)
		}
	}
	refused := func(name, why string) {
		t.Helper()
		a, err := ReadWithin(cwd, name, Policy{Own: []string{nodeDir}})
		if err == nil {
			t.Fatalf("ReadWithin(%q) accepted (%d bytes)", name, len(a.Data))
		}
		if why != "" && !strings.Contains(err.Error(), why) {
			t.Fatalf("ReadWithin(%q): %v (want %q)", name, err, why)
		}
	}
	ok("shots/a.png")
	ok(filepath.Join(cwd, "shots", "a.png"))
	ok("shots/../shots/a.png")

	refused("../outside/secret.png", "outside")
	refused(filepath.Join(outside, "secret.png"), "outside")
	refused(".", "")
	refused("", "")
	refused("notes.txt", "not a PNG")
	refused("shots", "not a regular file")
	refused("missing.png", "no such file")
	// The node's key: inside the working directory here, but never readable.
	refused(filepath.Join(nodeDir, "node.key"), "Switchworks's own directory")
	refused(".config/swk/node/disguised.png", "Switchworks's own directory")

	if runtime.GOOS == "windows" {
		return // symlinks need privileges there
	}
	// A symlink as the file itself, even to an image inside.
	if err := os.Symlink(filepath.Join(cwd, "shots", "a.png"), filepath.Join(cwd, "link.png")); err != nil {
		t.Fatal(err)
	}
	refused("link.png", "symlink")
	// A symlink to a file outside.
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(cwd, "escape.png")); err != nil {
		t.Fatal(err)
	}
	refused("escape.png", "outside")
	// A symlinked directory leading outside.
	if err := os.Symlink(outside, filepath.Join(cwd, "out")); err != nil {
		t.Fatal(err)
	}
	refused("out/secret.png", "outside")
	// A symlinked directory leading to the node's directory.
	if err := os.Symlink(nodeDir, filepath.Join(cwd, "nd")); err != nil {
		t.Fatal(err)
	}
	refused("nd/disguised.png", "Switchworks's own directory")
	// A symlinked directory leading back inside: refused too, since the
	// component could be swapped between the check and the open.
	if err := os.Symlink(filepath.Join(cwd, "shots"), filepath.Join(cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	refused("alias/a.png", "symlink")
	// A hard link to an image in the node's directory.
	if err := os.Link(filepath.Join(nodeDir, "disguised.png"), filepath.Join(cwd, "hard.png")); err == nil {
		refused("hard.png", "hard links")
	}
	// A working directory reached through a symlink: absolute paths in
	// either form work.
	linkCwd := filepath.Join(root, "worklink")
	if err := os.Symlink(cwd, linkCwd); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWithin(linkCwd, filepath.Join(cwd, "shots", "a.png"), Policy{}); err != nil {
		t.Fatalf("real path under a symlinked cwd: %v", err)
	}
	if _, err := ReadWithin(linkCwd, "shots/a.png", Policy{}); err != nil {
		t.Fatalf("relative path under a symlinked cwd: %v", err)
	}
	// A FIFO is not a regular file (and is never opened).
	if err := mkfifo(filepath.Join(cwd, "pipe.png")); err == nil {
		refused("pipe.png", "not a regular file")
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.png")
	if err := os.WriteFile(p, pngBytes(t, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ReadFile(p)
	if err != nil || a.Name != "x.png" || a.MIME != "image/png" {
		t.Fatalf("ReadFile: %+v %v", a, err)
	}
	if _, err := ReadFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
	txt := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(txt, []byte("hello"), 0o600)
	if _, err := ReadFile(txt); err == nil {
		t.Fatal("text accepted")
	}
}
