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

	a, err := ReadFileWithin(cwd, "out/fix.patch", Policy{Own: []string{nodeDir}})
	if err != nil || !bytes.Equal(a.Data, patch) || a.Name != "fix.patch" || a.MIME != "text/x-patch" {
		t.Fatalf("ReadFileWithin: %+v %v", a, err)
	}
	if _, err := ReadFileWithin(cwd, filepath.Join(cwd, "out", "fix.patch"), Policy{}); err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	refused := func(name, why string) {
		t.Helper()
		a, err := ReadFileWithin(cwd, name, Policy{Own: []string{nodeDir}})
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

// A session working in the home directory cannot attach the credentials
// a sandbox profile denies, nor dot-files, nor names that look like
// secrets; attach_allow lifts the last two only.
func TestReadFileWithinPolicy(t *testing.T) {
	home := t.TempDir()
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
	for _, f := range []string{".ssh/id_ed25519", ".ssh/config", ".aws/credentials", ".codex/auth.json", ".netrc",
		".config/gh/hosts.yml", ".config/other/notes.txt", ".bashrc", "proj/.env", "proj/.env.local", "proj/.env.example",
		"proj/keys/id_ed25519", "proj/keys/id_ed25519.pub", "proj/.github/ci.md", "proj/fix.patch", "notes.txt",
		"private/plan.txt", "sub/home/.profile", "sub/home/ok.txt", "Library/Keychains/x.txt"} {
		write(f, "text\n")
	}
	base := Policy{Deny: append(CredentialPaths(home), filepath.Join(home, "private")), Home: home}
	try := func(p Policy, dir, name string) error {
		t.Helper()
		_, err := ReadFileWithin(dir, name, p)
		return err
	}
	ok := func(p Policy, dir, name string) {
		t.Helper()
		if err := try(p, dir, name); err != nil {
			t.Errorf("%s in %s: %v", name, dir, err)
		}
	}
	refused := func(p Policy, dir, name, why string) {
		t.Helper()
		err := try(p, dir, name)
		if err == nil {
			t.Errorf("%s in %s accepted", name, dir)
		} else if !strings.Contains(err.Error(), why) {
			t.Errorf("%s in %s: %v (want %q)", name, dir, err, why)
		}
	}
	const creds, secret, dots = "holds credentials, or", "looks like it holds credentials", "dot-file"
	// Working in the home directory.
	for _, f := range []string{".ssh/id_ed25519", ".ssh/config", ".aws/credentials", ".codex/auth.json", ".netrc",
		".config/gh/hosts.yml", "private/plan.txt", filepath.Join(home, ".ssh", "id_ed25519"), "Library/Keychains/x.txt"} {
		refused(base, home, f, creds)
	}
	refused(base, home, ".bashrc", dots)
	refused(base, home, ".config/other/notes.txt", dots)
	refused(base, home, "proj/.github/ci.md", dots)
	refused(base, home, "proj/.env", secret)
	ok(base, home, "notes.txt")
	ok(base, home, "proj/fix.patch")
	// Above the home directory: dot-files below it are refused too.
	above := base
	above.Home = filepath.Join(home, "sub", "home")
	refused(above, home, "sub/home/.profile", dots)
	ok(above, home, "sub/home/ok.txt")
	// Inside a project: dot-files are fine, secrets are not.
	proj := filepath.Join(home, "proj")
	ok(base, proj, ".github/ci.md")
	ok(base, proj, ".env.example")
	ok(base, proj, "keys/id_ed25519.pub")
	refused(base, proj, ".env", secret)
	refused(base, proj, ".env.local", secret)
	refused(base, proj, "keys/id_ed25519", secret)
	// Working inside a denied directory.
	refused(base, filepath.Join(home, ".ssh"), "config", creds)
	// attach_allow lifts the name rules, never a deny.
	allow := base
	allow.Allow = []string{filepath.Join(home, ".bashrc"), filepath.Join(proj, ".env"), filepath.Join(home, ".ssh"), filepath.Join(home, "private")}
	ok(allow, home, ".bashrc")
	ok(allow, proj, ".env")
	refused(allow, proj, ".env.local", secret)
	refused(allow, home, ".ssh/config", creds)
	refused(allow, home, "private/plan.txt", creds)

	if runtime.GOOS == "windows" {
		return // symlinks need privileges there
	}
	// A deny path given through a symlink still matches, by name and by
	// identity.
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	viaLink := Policy{Deny: []string{filepath.Join(link, ".netrc"), filepath.Join(link, "private")}}
	refused(viaLink, home, ".netrc", creds)
	refused(viaLink, home, "private/plan.txt", creds)
	refused(viaLink, proj, filepath.Join(link, "private", "plan.txt"), "outside")
	// A hard link to a credential inside a project.
	if err := os.Link(filepath.Join(home, ".aws", "credentials"), filepath.Join(proj, "aws.txt")); err == nil {
		refused(base, proj, "aws.txt", "hard links")
	}
}

// A hard link inside the working directory to a file in Switchworks's
// directory is refused on every OS: on Windows the link count comes from
// the open handle.
func TestReadFileWithinHardLink(t *testing.T) {
	root := t.TempDir()
	cwd, nodeDir := filepath.Join(root, "work"), filepath.Join(root, "node")
	for _, d := range []string{cwd, nodeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(nodeDir, "cli.json")
	if err := os.WriteFile(key, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(key, filepath.Join(cwd, "copy.json")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	_, err := ReadFileWithin(cwd, "copy.json", Policy{Own: []string{nodeDir}})
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("hard link to cli.json: %v", err)
	}
}

func TestSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, ".ENV": true, ".env.production": true, "prod.env": true, ".envrc": true, ".env.example": false,
		"id_rsa": true, "id_ed25519_sk": true, "id_ecdsa.pub": false, ".netrc": true, ".npmrc": true, "auth.json": true,
		".ssh": true, "credentials": true, "notes.txt": false, ".github": false, "environment.md": false,
	} {
		if got := SecretName(name); got != want {
			t.Errorf("SecretName(%q) = %v, want %v", name, got, want)
		}
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
