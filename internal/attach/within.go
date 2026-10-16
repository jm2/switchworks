// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/switchyard/internal/model"
)

// ReadWithin reads an image file an agent names for the fleet tools.
//
// The fleet tools run as the user, outside any sandbox the agent has, so
// an unchecked path would let a sandboxed agent send out files it cannot
// read itself (the node's key, say). The file must therefore be:
//   - inside dir (the session's working directory), with symlinks resolved;
//     the open goes through os.Root, so no component can lead out of dir;
//   - a regular file, not a symlink, device, pipe or other special file;
//   - not inside any of deny (the node's own directory), however it is
//     reached;
//   - at most MaxImageBytes, and a PNG, JPEG, WebP or GIF by its bytes.
//
// name may be relative to dir or absolute.
func ReadWithin(dir, name string, deny []string) (model.Attachment, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return model.Attachment{}, errors.New("empty or invalid image path")
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return model.Attachment{}, errors.New("this session has no working directory to read images from")
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("working directory: %w", err)
	}
	rel, ok := relInside(name, dir, realDir)
	if !ok {
		return model.Attachment{}, fmt.Errorf("%s is outside this session's working directory (%s); images must be files under it", name, dir)
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
			return model.Attachment{}, fmt.Errorf("%s is inside Switchyard's own directory and cannot be attached", name)
		}
	}
	root, err := os.OpenRoot(realDir)
	if err != nil {
		return model.Attachment{}, err
	}
	defer root.Close()
	fi, err := root.Lstat(rel)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return model.Attachment{}, fmt.Errorf("%s is a symlink; attach the file itself", name)
	case !fi.Mode().IsRegular():
		return model.Attachment{}, fmt.Errorf("%s is not a regular file", name)
	case fi.Size() > MaxImageBytes:
		return model.Attachment{}, fmt.Errorf("%s is %d bytes; the limit is %d (5 MiB)", name, fi.Size(), MaxImageBytes)
	}
	f, err := root.Open(rel)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %w", name, notExist(err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(fi, st) {
		return model.Attachment{}, fmt.Errorf("%s changed while it was being read", name)
	}
	return readImage(f, filepath.Base(rel))
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
	for _, base := range []string{dir, realDir} {
		rel, err := filepath.Rel(base, filepath.Clean(name))
		if err == nil && rel != "." && !escapes(rel) && !filepath.IsAbs(rel) {
			return rel, true
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
