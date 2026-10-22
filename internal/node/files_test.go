// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

// The sender's node reads "files" like "images", into "file_attachments".
func TestResolveFiles(t *testing.T) {
	cwd := t.TempDir()
	nodeDir := filepath.Join(cwd, ".config", "swk", "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/x b/x\n")
	_ = os.WriteFile(filepath.Join(cwd, "fix.patch"), patch, 0o600)
	_ = os.WriteFile(filepath.Join(nodeDir, "node.key"), []byte("-----BEGIN EC PRIVATE KEY-----\n"), 0o600)
	img := testPNG(t, 2)
	_ = os.WriteFile(filepath.Join(cwd, "shot.png"), img, 0o600)
	n := &Node{dir: nodeDir}
	s := &session{n: n, rec: record{ID: "s1", Spec: adapter.StartSpec{Cwd: cwd}}}

	out, err := n.resolveImages(s, []byte(`{"to":"b","text":"am","files":["fix.patch"],"images":["shot.png"]}`), filesArg)
	if err != nil {
		t.Fatal(err)
	}
	var args struct {
		Files           []string           `json:"files"`
		FileAttachments []model.Attachment `json:"file_attachments"`
		Attachments     []model.Attachment `json:"attachments"`
	}
	if err := json.Unmarshal(out, &args); err != nil {
		t.Fatal(err)
	}
	if args.Files != nil || len(args.FileAttachments) != 1 || !bytes.Equal(args.FileAttachments[0].Data, patch) ||
		args.FileAttachments[0].MIME != "text/x-patch" || len(args.Attachments) != 1 {
		t.Fatalf("resolved %s", out)
	}
	for body, want := range map[string]string{
		`{"files":[".config/swk/node/node.key"]}`:        "Switchworks's own directory",
		`{"files":["../x.txt"]}`:                         "outside",
		`{"files":["shot.png"]}`:                         "not a text file",
		`{"files":"fix.patch"}`:                          "list of file paths",
		`{"files":["a","b","c","d","e"]}`:                "at most 4",
		`{"file_attachments":[{"data":"AAAA"}]}`:         "by file path",
		`{"File_Attachments":[{"data":"AAAA"}]}`:         "by file path",
		`{"Files":["fix.patch"]}`:                        `spell it "files"`,
		`{"images":["fix.patch"],"files":["fix.patch"]}`: "not a PNG",
	} {
		if _, err := n.resolveImages(s, []byte(body), filesArg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
	// Without filesArg (a spawn), "files" passes through for the
	// coordinator to refuse.
	if out, err := n.resolveImages(s, []byte(`{"files":["fix.patch"]}`)); err != nil || string(out) != `{"files":["fix.patch"]}` {
		t.Fatalf("spawn body: %s %v", out, err)
	}
}

// The receiving node writes a message's files to a folder of their own,
// names them in the header, and removes the folder a while after delivery.
func TestReceivedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "files")
	n := &Node{dir: t.TempDir(), cfg: Config{FilesDir: root}, sessions: map[string]*session{}}
	files, err := attach.NormalizeFiles([]model.Attachment{{Name: "fix.patch", Data: []byte("diff\n")}, {Name: "../fix.patch", Data: []byte("two\n")},
		{Name: "CON", Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := n.writeFiles("s1", "m1", files)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "s1", "m1")
	for i, want := range []string{"fix.patch", "fix-2.patch", "_CON"} {
		if refs[i].Path != filepath.Join(dir, want) {
			t.Fatalf("file %d at %s", i, refs[i].Path)
		}
		if b, err := os.ReadFile(refs[i].Path); err != nil || !bytes.Equal(b, files[i].Data) {
			t.Fatalf("file %d: %q %v", i, b, err)
		}
	}
	// Written again (a redelivery that was never queued): a fresh folder.
	if _, err := n.writeFiles("s1", "m1", files[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_CON")); err == nil {
		t.Fatal("old folder kept")
	}
	for _, bad := range [][2]string{{"../s", "m"}, {"s1", "m/../.."}, {"", "m"}, {"s1", ".."}} {
		if _, err := n.writeFiles(bad[0], bad[1], files[:1]); err == nil {
			t.Errorf("ids %q accepted", bad)
		}
	}

	text := formatMessage(queuedMsg{ID: "m1", From: "devbox/win", Kind: model.MsgNotify, Text: "please git am", Files: refs[:1]})
	want := "(1 file attached: " + refs[0].Path + " (text/x-patch, 5 B).)\nplease git am"
	if !strings.HasSuffix(text, want) || !strings.HasPrefix(text, "[switchworks msg=m1 from=devbox/win kind=notify]\n") {
		t.Fatalf("formatted:\n%s", text)
	}
	if got := formatMessage(queuedMsg{ID: "m1", From: "user:jm", Owner: true, Text: "hi", Files: refs[:2]}); !strings.HasPrefix(got, "(2 files attached: ") ||
		!strings.HasSuffix(got, ".)\nhi") {
		t.Fatalf("owner's message: %s", got)
	}

	// The sweep: a queued message's folder is kept (and refreshed), a
	// delivered one goes filesKeep after it was last refreshed.
	if _, err := n.writeFiles("s1", "m2", files[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writeFiles("s2", "m3", files[:1]); err != nil {
		t.Fatal(err)
	}
	n.sessions["s1"] = &session{n: n, rec: record{ID: "s1", Queue: []queuedMsg{{ID: "m2", Files: refs[:1]}}}}
	old := time.Now().Add(-filesKeep - time.Hour)
	for _, p := range []string{dir, filepath.Join(root, "s1", "m2"), filepath.Join(root, "s2", "m3")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, "s1", "stray.txt"), []byte("x"), 0o600) // not named like an id folder
	n.pruneFiles(time.Now())
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("delivered message's files kept")
	}
	if fi, err := os.Stat(filepath.Join(root, "s1", "m2")); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("queued message's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "s2")); err == nil {
		t.Fatal("empty session folder kept")
	}
	if _, err := os.Stat(filepath.Join(root, "s1", "stray.txt")); err != nil {
		t.Fatal("stray file removed")
	}
}

// A node with nowhere to put files does not offer them.
func TestFilesFeature(t *testing.T) {
	n := &Node{dir: t.TempDir(), cfg: Config{FilesDir: t.TempDir()}, sessions: map[string]*session{}}
	if h := n.hello(); !containsID(h.Features, "files") {
		t.Fatalf("features %v", h.Features)
	}
}
