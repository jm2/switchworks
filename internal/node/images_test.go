// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

func testPNG(t *testing.T, w int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// resolveImages reads an agent's image paths under its session's working
// directory only, and never from the node's own directory.
func TestResolveImages(t *testing.T) {
	cwd := t.TempDir()
	nodeDir := filepath.Join(cwd, ".config", "swk", "node") // a session working in $HOME
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	img := testPNG(t, 4)
	_ = os.WriteFile(filepath.Join(cwd, "shot.png"), img, 0o600)
	_ = os.WriteFile(filepath.Join(nodeDir, "node.key"), []byte("-----BEGIN EC PRIVATE KEY-----\n"), 0o600)
	_ = os.WriteFile(filepath.Join(nodeDir, "state.png"), img, 0o600)
	n := &Node{dir: nodeDir}
	s := &session{n: n, rec: record{ID: "s1", Spec: adapter.StartSpec{Cwd: cwd}}}

	out, err := n.resolveImages(s, []byte(`{"to":"buildbox/b","text":"look","images":["shot.png"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var args struct {
		To          string             `json:"to"`
		Images      []string           `json:"images"`
		Attachments []model.Attachment `json:"attachments"`
	}
	if err := json.Unmarshal(out, &args); err != nil {
		t.Fatal(err)
	}
	if args.To != "buildbox/b" || args.Images != nil || len(args.Attachments) != 1 || !bytes.Equal(args.Attachments[0].Data, img) ||
		args.Attachments[0].Name != "shot.png" || args.Attachments[0].SHA256 != attach.SHA256(img) {
		t.Fatalf("resolved %s", out)
	}
	// Without images the body passes through untouched.
	if out, err := n.resolveImages(s, []byte(`{"to":"x","text":"y"}`)); err != nil || string(out) != `{"to":"x","text":"y"}` {
		t.Fatalf("plain body: %s %v", out, err)
	}
	keyPath, _ := json.Marshal(filepath.Join(nodeDir, "node.key"))
	for body, want := range map[string]string{
		`{"images":[` + string(keyPath) + `]}`:      "Switchworks's own directory",
		`{"images":[".config/swk/node/state.png"]}`: "Switchworks's own directory",
		`{"images":["../shot.png"]}`:                "outside",
		`{"images":"shot.png"}`:                     "list of file paths",
		`{"images":["a","b","c","d","e"]}`:          "at most 4",
		`{"attachments":[{"data":"AAAA"}]}`:         "by file path",
		`{"Attachments":[{"data":"AAAA"}]}`:         "by file path",
		`{"IMAGES":["shot.png"]}`:                   `spell it "images"`,
	} {
		if _, err := n.resolveImages(s, []byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
	// A session confined to a root its cwd has left (a swapped symlink) is
	// refused before any file is read.
	s.rec.Spec.CwdRoot = t.TempDir()
	if _, err := n.resolveImages(s, []byte(`{"images":["shot.png"]}`)); err == nil {
		t.Fatal("cwd outside its root accepted")
	}
}

func TestQueuedImagesOnDisk(t *testing.T) {
	n := &Node{dir: t.TempDir()}
	s := &session{n: n, rec: record{ID: "s1"}}
	imgs, err := attach.Normalize([]model.Attachment{{Name: "a.png", Data: testPNG(t, 2)}, {Name: "b.png", Data: testPNG(t, 3)}})
	if err != nil {
		t.Fatal(err)
	}
	metas, err := n.storeImages("s1", imgs)
	if err != nil {
		t.Fatal(err)
	}
	if metas[0].Data != nil || metas[0].SHA256 != imgs[0].SHA256 {
		t.Fatalf("metas %+v", metas)
	}
	// state.json keeps the metadata only.
	b, _ := json.Marshal(queuedMsg{ID: "m", Attachments: metas})
	if bytes.Contains(b, []byte(`"data"`)) {
		t.Fatalf("queued message carries data: %s", b)
	}
	back, err := n.loadImages("s1", metas)
	if err != nil || !bytes.Equal(back[1].Data, imgs[1].Data) {
		t.Fatalf("load: %v", err)
	}
	// Only the first message is still queued: the other image goes.
	s.rec.Queue = []queuedMsg{{ID: "m1", Attachments: metas[:1]}}
	n.gcImages(s)
	if _, err := n.loadImages("s1", metas[:1]); err != nil {
		t.Fatalf("queued image removed: %v", err)
	}
	if _, err := n.loadImages("s1", metas[1:]); err == nil {
		t.Fatal("unqueued image kept")
	}
	// A tampered file is refused.
	_ = os.WriteFile(filepath.Join(n.attachDir("s1"), metas[0].SHA256), []byte("x"), 0o600)
	if _, err := n.loadImages("s1", metas[:1]); err == nil {
		t.Fatal("corrupt image loaded")
	}
	n.dropImages("s1")
	if _, err := os.Stat(n.attachDir("s1")); !os.IsNotExist(err) {
		t.Fatal("attachments kept after stop")
	}
}

func TestFormatMessageImages(t *testing.T) {
	m := queuedMsg{ID: "m1", From: "devbox/game", Kind: "notify", Text: "does it render?",
		Attachments: []model.Attachment{{Name: "level1.png", MIME: "image/png", Size: 2048, SHA256: strings.Repeat("a", 64)}}}
	out := formatMessage(m)
	if !strings.Contains(out, "(1 image attached, in this turn's input: level1.png (image/png, 2 KiB).)\n") ||
		!strings.HasSuffix(out, "does it render?") {
		t.Fatalf("formatted:\n%s", out)
	}
	// From a person, passed through as before.
	m.From = "user:web"
	if formatMessage(m) != "does it render?" {
		t.Fatal("a person's message changed")
	}
}

func TestHelloImages(t *testing.T) {
	got := takesImages([]string{"agy", "codex", "goose"})
	if strings.Join(got, ",") != "codex,goose" {
		t.Fatalf("images %v", got)
	}
}
