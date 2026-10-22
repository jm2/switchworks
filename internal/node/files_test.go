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

// An agent working in the home directory cannot attach credentials, the
// paths Codex's permission profiles deny, attach_deny, nor received
// files; attach_allow lifts the dot-file rule.
func TestAttachPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".codex/config.toml", "model = \"x\"\n[permissions.swk.filesystem]\n\"~/secrets\" = \"deny\"\n\"~/work/vault/k.txt\" = \"deny\"\n")
	for _, f := range []string{"secrets/a.txt", "work/vault/k.txt", "work/private/p.txt", "work/ok.txt", ".ssh/id_ed25519",
		".aws/credentials", ".bashrc", ".profile", "recv/s1/m1-X/fix.patch"} {
		write(f, "text\n")
	}
	n := &Node{dir: filepath.Join(home, "swk-node"), cfg: Config{FilesDir: filepath.Join(home, "recv"),
		AttachDeny: []string{"~/work/private"}, AttachAllow: []string{"~/.bashrc"}}}
	s := &session{n: n, rec: record{ID: "s1", Spec: adapter.StartSpec{Cwd: home}}}
	send := func(name string) error {
		b, _ := json.Marshal(map[string]any{"to": "b", "text": "x", "files": []string{name}})
		_, err := n.resolveImages(s, b, filesArg)
		return err
	}
	for name, want := range map[string]string{
		"secrets/a.txt":            "holds credentials",
		"work/vault/k.txt":         "holds credentials",
		"work/private/p.txt":       "attach_deny",
		".ssh/id_ed25519":          "holds credentials",
		".aws/credentials":         "holds credentials",
		".codex/config.toml":       "holds credentials",
		"recv/s1/m1-X/fix.patch":   "holds credentials",
		".profile":                 "dot-file",
		"~/.ssh/id_ed25519":        "no such file", // never expanded: a name under the cwd
		"work/../.aws/credentials": "holds credentials",
	} {
		if err := send(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	for _, name := range []string{"work/ok.txt", ".bashrc"} {
		if err := send(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Images take the same path.
	if _, err := n.resolveImages(s, []byte(`{"images":[".ssh/id_ed25519"]}`)); err == nil || !strings.Contains(err.Error(), "holds credentials") {
		t.Errorf("image from ~/.ssh: %v", err)
	}
	for _, bad := range []Config{{AttachDeny: []string{"work"}}, {AttachAllow: []string{"./x"}}} {
		if checkAttachPaths(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := checkAttachPaths(Config{AttachDeny: []string{"~/x", home}, AttachAllow: []string{"~"}}); err != nil {
		t.Error(err)
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
	// A folder of the message's own, named with a random part.
	dir := filepath.Dir(refs[0].Path)
	if filepath.Dir(dir) != filepath.Join(root, "s1") || !strings.HasPrefix(filepath.Base(dir), "m1-") || len(filepath.Base(dir)) < 3+26 ||
		!safeID(filepath.Base(dir)) {
		t.Fatalf("folder %s", dir)
	}
	for i, want := range []string{"fix.patch", "fix-2.patch", "_CON"} {
		if refs[i].Path != filepath.Join(dir, want) {
			t.Fatalf("file %d at %s", i, refs[i].Path)
		}
		if b, err := os.ReadFile(refs[i].Path); err != nil || !bytes.Equal(b, files[i].Data) {
			t.Fatalf("file %d: %q %v", i, b, err)
		}
	}
	// Written again (a redelivery that was never queued): a fresh folder.
	again, err := n.writeFiles("s1", "m1", files[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil || filepath.Dir(again[0].Path) == dir {
		t.Fatal("old folder kept")
	}
	dir = filepath.Dir(again[0].Path)
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
	m2, err := n.writeFiles("s1", "m2", files[:1])
	if err != nil {
		t.Fatal(err)
	}
	m3, err := n.writeFiles("s2", "m3", files[:1])
	if err != nil {
		t.Fatal(err)
	}
	m2Dir := filepath.Dir(m2[0].Path)
	n.sessions["s1"] = &session{n: n, rec: record{ID: "s1", Queue: []queuedMsg{{ID: "m2", Files: m2}}}}
	old := time.Now().Add(-filesKeep - time.Hour)
	for _, p := range []string{dir, m2Dir, filepath.Dir(m3[0].Path)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, "s1", "stray.txt"), []byte("x"), 0o600) // not named like an id folder
	n.pruneFiles(time.Now())
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("delivered message's files kept")
	}
	if fi, err := os.Stat(m2Dir); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("queued message's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "s2")); err == nil {
		t.Fatal("empty session folder kept")
	}
	if _, err := os.Stat(filepath.Join(root, "s1", "stray.txt")); err != nil {
		t.Fatal("stray file removed")
	}
}

// Received files are capped all sessions together: a message whose files
// would pass files_quota_mib is refused, and fits again once older ones
// are swept.
func TestReceivedFilesQuota(t *testing.T) {
	root := filepath.Join(t.TempDir(), "files")
	n := &Node{dir: t.TempDir(), cfg: Config{FilesDir: root, FilesQuotaMiB: 1}, sessions: map[string]*session{}}
	big, err := attach.NormalizeFiles([]model.Attachment{{Name: "a.log", Data: bytes.Repeat([]byte("x"), 600<<10)}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := n.writeFiles("s1", "m1", big)
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.writeFiles("s2", "m2", big)
	if err == nil || !strings.Contains(err.Error(), "files_quota_mib") {
		t.Fatalf("over the quota: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "s2")); err == nil {
		t.Fatal("refused files written")
	}
	// A redelivery of the same message replaces its own folder, so it is
	// not counted against itself.
	if first, err = n.writeFiles("s1", "m1", big); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	old := time.Now().Add(-filesKeep - time.Hour)
	_ = os.Chtimes(filepath.Dir(first[0].Path), old, old)
	n.pruneFiles(time.Now())
	if _, err := n.writeFiles("s2", "m2", big); err != nil {
		t.Fatalf("after the sweep: %v", err)
	}
	// -1 turns the cap off.
	n.cfg.FilesQuotaMiB = -1
	if _, err := n.writeFiles("s3", "m3", big); err != nil {
		t.Fatalf("no quota: %v", err)
	}
	if (&Node{}).filesQuota() != defaultFilesQuotaMiB<<20 {
		t.Fatal("default quota")
	}
}

// A node with nowhere to put files does not offer them.
func TestFilesFeature(t *testing.T) {
	n := &Node{dir: t.TempDir(), cfg: Config{FilesDir: t.TempDir()}, sessions: map[string]*session{}}
	if h := n.hello(); !containsID(h.Features, "files") {
		t.Fatalf("features %v", h.Features)
	}
}
