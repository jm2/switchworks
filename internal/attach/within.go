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
// read itself (the node's key, say). The file must therefore be:
//   - inside dir (the session's working directory), reached without any
//     symlink below dir: each component is opened through the one before
//     it and checked to be the directory that was looked at;
//   - a regular file with no other hard links, not a symlink, device, pipe
//     or other special file;
//   - not inside any of deny (the node's own directory), compared by
//     identity, so another spelling of the name does not get past it;
//   - at most MaxImageBytes, and a PNG, JPEG, WebP or GIF by its bytes.
//
// name may be relative to dir or absolute.
func ReadWithin(dir, name string, deny []string) (model.Attachment, error) {
	return readWithin(dir, name, deny, imageKind)
}

// ReadFileWithin reads a text file an agent attaches to a message, with
// the same path checks as ReadWithin: at most MaxFileBytes, and text by its
// bytes (see SniffText).
func ReadFileWithin(dir, name string, deny []string) (model.Attachment, error) {
	return readWithin(dir, name, deny, fileKind)
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

func readWithin(dir, name string, deny []string, k kind) (model.Attachment, error) {
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
	for _, d := range deny {
		if d == "" {
			continue
		}
		rd := d
		if r, err := filepath.EvalSymlinks(d); err == nil {
			rd = r
		}
		if within(rd, real) || within(filepath.Clean(d), filepath.Join(dir, rel)) {
			return model.Attachment{}, fmt.Errorf("%s is inside Switchworks's own directory and cannot be attached", name)
		}
	}
	return openConfined(realDir, rel, name, deny, k)
}

// openConfined opens realDir/rel one component at a time, each through the
// directory opened before it. It refuses any symlink (or junction) on the
// way and checks that every directory it opens is the one it looked at, so
// swapping a component for a symlink mid-read is caught. Directories are
// compared with deny by identity, not by name, which also covers names
// that differ only in case on macOS and Windows.
func openConfined(realDir, rel, name string, deny []string, k kind) (model.Attachment, error) {
	var denied []os.FileInfo
	for _, d := range deny {
		if d == "" {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			denied = append(denied, fi)
		}
	}
	isDenied := func(fi os.FileInfo) bool {
		for _, d := range denied {
			if os.SameFile(d, fi) {
				return true
			}
		}
		return false
	}
	inOwn := fmt.Errorf("%s is inside Switchworks's own directory and cannot be attached", name)
	// The working directory itself, and every directory above it.
	for d := realDir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && isDenied(fi) {
			return model.Attachment{}, inOwn
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
		if isDenied(fi) {
			return model.Attachment{}, inOwn
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
	if err != nil || !os.SameFile(fi, st) || !st.Mode().IsRegular() || hardLinked(st) {
		return model.Attachment{}, fmt.Errorf("%s changed while it was being read", name)
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
