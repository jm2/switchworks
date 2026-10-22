// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/switchworks/internal/model"
)

// ReadWithin reads an image file an agent names for the fleet tools.
//
// The fleet tools run as the user, outside any sandbox the agent has, so
// an unchecked path would let a sandboxed agent send out files it cannot
// read itself (the node's key, ~/.ssh). The file must therefore be:
//   - inside dir (the session's working directory), reached without any
//     symlink below dir: each component is opened through the one before
//     it and checked to be the directory that was looked at;
//   - a regular file with no other hard links, not a symlink, device, pipe
//     or other special file;
//   - not inside any of p.Own or p.Deny, compared by identity, so another
//     spelling of the name does not get past it;
//   - not named like a credential (SecretName), and, when dir is p.Home or
//     above it, not a dot-file or inside a dot-directory, unless it is
//     under one of p.Allow;
//   - at most MaxImageBytes, and a PNG, JPEG, WebP or GIF by its bytes.
//
// name may be relative to dir or absolute.
func ReadWithin(dir, name string, p Policy) (model.Attachment, error) {
	return readWithin(dir, name, p, imageKind)
}

// ReadFileWithin reads a text file an agent attaches to a message, with
// the same path checks as ReadWithin: at most MaxFileBytes, and text by its
// bytes (see SniffText).
func ReadFileWithin(dir, name string, p Policy) (model.Attachment, error) {
	return readWithin(dir, name, p, fileKind)
}

// Policy is what an agent may not attach, beyond the working directory.
type Policy struct {
	// Own is Switchworks's own directories (the node's, cli.json's).
	Own []string
	// Deny is other files and directories that hold credentials (see
	// CredentialPaths), or that node.json denies.
	Deny []string
	// Allow lifts the SecretName and dot-file rules (never Own or Deny)
	// for the files and directories it lists.
	Allow []string
	// Home is the user's home directory: a session working in it, or
	// above it, cannot attach dot-files, where most tools keep their
	// logins.
	Home string
}

// kind is what readWithin reads: images or text files.
type kind struct {
	noun  string // "image" or "file", for messages
	limit int64
	read  func(r io.Reader, name string) (model.Attachment, error)
}

var (
	imageKind = kind{"image", MaxImageBytes, readImage}
	fileKind  = kind{"file", MaxFileBytes, readText}
)

func readWithin(dir, name string, p Policy, k kind) (model.Attachment, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return model.Attachment{}, fmt.Errorf("empty or invalid %s path", k.noun)
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return model.Attachment{}, fmt.Errorf("this session has no working directory to read %ss from", k.noun)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("working directory: %w", err)
	}
	rel, ok := relInside(name, dir, realDir)
	if !ok {
		return model.Attachment{}, fmt.Errorf("%s is outside this session's working directory (%s); %ss must be files under it", name, dir, k.noun)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(realDir, rel))
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
	}
	if !within(realDir, real) {
		return model.Attachment{}, fmt.Errorf("%s leads outside this session's working directory", name)
	}
	named := filepath.Join(dir, rel)
	for _, d := range p.Own {
		if underName(d, real, named) {
			return model.Attachment{}, errOwn(name)
		}
	}
	for _, d := range p.Deny {
		if underName(d, real, named) {
			return model.Attachment{}, errDenied(name)
		}
	}
	if !allowed(p.Allow, real, named) {
		dots := aboveHome(realDir, p.Home)
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if SecretName(part) {
				return model.Attachment{}, fmt.Errorf("%s looks like it holds credentials (%s) and cannot be attached; list it in attach_allow in node.json to attach it anyway", name, part)
			}
			if dots && strings.HasPrefix(part, ".") {
				return model.Attachment{}, fmt.Errorf("%s is a dot-file or inside a dot-directory (%s), and this session works in your home directory or above it, where those hold logins; list it in attach_allow in node.json to attach it anyway", name, part)
			}
		}
	}
	return openConfined(realDir, rel, name, p, k)
}

func errOwn(name string) error {
	return fmt.Errorf("%s is inside Switchworks's own directory and cannot be attached", name)
}

func errDenied(name string) error {
	return fmt.Errorf("%s is in a location that holds credentials, or that attach_deny in node.json lists, and cannot be attached", name)
}

// underName reports whether real (the file, resolved) or named (as the
// agent spelled it, below the working directory) is d or below it. This
// is the check by name; openConfined repeats it by identity.
func underName(d, real, named string) bool {
	if d == "" {
		return false
	}
	rd := d
	if r, err := filepath.EvalSymlinks(d); err == nil {
		rd = r
	}
	return within(rd, real) || within(filepath.Clean(d), named)
}

// allowed reports whether the file is under one of allow.
func allowed(allow []string, real, named string) bool {
	for _, a := range allow {
		if underName(a, real, named) {
			return true
		}
	}
	return false
}

// aboveHome reports whether realDir is home or one of its ancestors,
// compared by identity. Unknown counts as yes.
func aboveHome(realDir, home string) bool {
	if home == "" {
		return false
	}
	dfi, err := os.Stat(realDir)
	if err != nil {
		return true
	}
	h := home
	if r, err := filepath.EvalSymlinks(home); err == nil {
		h = r
	}
	for d := filepath.Clean(h); ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && os.SameFile(dfi, fi) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// SecretName reports whether a file or directory name is one that
// usually holds credentials wherever it is: .env files, SSH keys, token
// files. Compared without regard to case.
func SecretName(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case ".env", ".envrc", ".netrc", "_netrc", ".git-credentials", ".npmrc", ".pypirc", ".pgpass",
		".ssh", ".gnupg", ".aws", ".docker", ".kube", ".password-store", ".vault-token",
		"credentials", "credentials.json", "credentials.toml", "auth.json":
		return true
	}
	switch {
	case n == ".env.example", n == ".env.sample", n == ".env.template":
		return false // templates without the values
	case strings.HasPrefix(n, ".env."), strings.HasSuffix(n, ".env"):
		return true
	case strings.HasPrefix(n, "id_rsa"), strings.HasPrefix(n, "id_dsa"), strings.HasPrefix(n, "id_ecdsa"), strings.HasPrefix(n, "id_ed25519"):
		return !strings.HasSuffix(n, ".pub")
	}
	return false
}

// openConfined opens realDir/rel one component at a time, each through the
// directory opened before it. It refuses any symlink (or junction) on the
// way and checks that every directory it opens is the one it looked at, so
// swapping a component for a symlink mid-read is caught. Directories and
// the file are compared with p.Own and p.Deny by identity, not by name,
// which also covers names that differ only in case on macOS and Windows.
func openConfined(realDir, rel, name string, p Policy, k kind) (model.Attachment, error) {
	type deny struct {
		fi  os.FileInfo
		err error
	}
	var denied []deny
	add := func(ds []string, err error) {
		for _, d := range ds {
			if d == "" {
				continue
			}
			if fi, e := os.Stat(d); e == nil {
				denied = append(denied, deny{fi, err})
			}
		}
	}
	add(p.Own, errOwn(name))
	add(p.Deny, errDenied(name))
	isDenied := func(fi os.FileInfo) error {
		for _, d := range denied {
			if os.SameFile(d.fi, fi) {
				return d.err
			}
		}
		return nil
	}
	// The working directory itself, and every directory above it.
	for d := realDir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil {
			if err := isDenied(fi); err != nil {
				return model.Attachment{}, err
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}

	root, err := os.OpenRoot(realDir)
	if err != nil {
		return model.Attachment{}, err
	}
	defer func() { root.Close() }()
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		fi, err := root.Lstat(part)
		if err != nil {
			return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
		}
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 || !fi.IsDir() {
			return model.Attachment{}, fmt.Errorf("%s goes through a symlink or special directory (%s); name the file by its real path", name, part)
		}
		if err := isDenied(fi); err != nil {
			return model.Attachment{}, err
		}
		sub, err := root.OpenRoot(part)
		if err != nil {
			return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
		}
		st, err := sub.Stat(".")
		if err != nil || !os.SameFile(fi, st) {
			sub.Close()
			return model.Attachment{}, fmt.Errorf("%s changed while it was being read", name)
		}
		root.Close()
		root = sub
	}
	base := parts[len(parts)-1]
	fi, err := root.Lstat(base)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return model.Attachment{}, fmt.Errorf("%s is a symlink; attach the file itself", name)
	case !fi.Mode().IsRegular():
		return model.Attachment{}, fmt.Errorf("%s is not a regular file", name)
	case fi.Size() > k.limit:
		return model.Attachment{}, fmt.Errorf("%s is %d bytes; the limit is %d (%d MiB)", name, fi.Size(), k.limit, k.limit>>20)
	case hardLinked(fi):
		// Another name for it may be anywhere, Switchworks's directories
		// included.
		return model.Attachment{}, fmt.Errorf("%s has other hard links; attach a copy", name)
	}
	f, err := root.OpenFile(base, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(fi, st) || !st.Mode().IsRegular() {
		return model.Attachment{}, fmt.Errorf("%s changed while it was being read", name)
	}
	if err := isDenied(st); err != nil {
		return model.Attachment{}, err
	}
	// Asked of the open file: only that reports link counts on Windows.
	if linked, err := openHardLinked(f, st); err != nil || linked {
		return model.Attachment{}, fmt.Errorf("%s has other hard links; attach a copy", name)
	}
	return k.read(f, filepath.Base(rel))
}

// relInside returns name relative to dir (or to realDir, its resolved
// form), if it is below it.
func relInside(name, dir, realDir string) (string, bool) {
	if filepath.VolumeName(name) != "" && !filepath.IsAbs(name) {
		return "", false // "C:foo" on Windows
	}
	if !filepath.IsAbs(name) {
		rel := filepath.Clean(name)
		return rel, rel != "." && !escapes(rel)
	}
	names := []string{filepath.Clean(name)}
	// The name may reach the directory through a different symlink
	// (/var vs /private/var on macOS): compare its resolved parent too.
	// What it resolves to is checked again, and opened through os.Root.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(name)); err == nil {
		names = append(names, filepath.Join(parent, filepath.Base(name)))
	}
	for _, n := range names {
		for _, base := range []string{dir, realDir} {
			rel, err := filepath.Rel(base, n)
			if err == nil && rel != "." && !escapes(rel) && !filepath.IsAbs(rel) {
				return rel, true
			}
		}
	}
	return "", false
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

// within reports whether p is root or below it (both cleaned, absolute).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !escapes(rel)
}

func notExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no such file")
	}
	return err
}
