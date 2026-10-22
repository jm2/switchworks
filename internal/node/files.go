// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/version"
)

// Files received with messages (patches, logs):
//
//   - When the node accepts a message it writes each file to
//     <files dir>/<session>/<message>/<name> (directories 0700, files
//     0600; the name is the sender's, made safe by attach.DiskNames), and
//     the message names their absolute paths, so the agent reads them
//     with its own tools and nobody copies them by hand.
//   - <files dir> is <user cache dir>/switchworks/files: ~/.cache on
//     Linux, ~/Library/Caches on macOS, %LocalAppData% on Windows, or
//     files_dir in node.json. Not the node's directory: a Codex permission
//     profile that keeps the node key from agents denies reads of the
//     whole Switchworks config directory (~/.config/swk, see
//     docs/harnesses.md), so a sandboxed agent could not read them there.
//     Not the session's working directory either, which they would clutter
//     (and a repository would pick up). Codex's sandboxes let commands read
//     outside the workspace, so the agent reads them where they are.
//   - A message's folder is removed filesKeep after the message left the
//     queue: while it is queued, the hourly sweep keeps it fresh.

const (
	filesKeep  = 7 * 24 * time.Hour
	filesSweep = time.Hour
)

// fileRef is a received file as written for the session.
type fileRef struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// filesRoot is the directory received files go in, or "" when this node
// has none (then it does not offer wire.FeatureFiles).
func (n *Node) filesRoot() string {
	if n.cfg.FilesDir != "" {
		abs, err := filepath.Abs(n.cfg.FilesDir)
		if err != nil {
			return ""
		}
		return abs
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, version.Slug, "files")
}

// safeID reports whether a session or message id can name a directory.
func safeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// writeFiles writes a message's files (normalized, with data) to a fresh
// folder of their own and returns where they are.
func (n *Node) writeFiles(sessionID, msgID string, files []model.Attachment) ([]fileRef, error) {
	root := n.filesRoot()
	switch {
	case root == "":
		return nil, errors.New("this node has no directory for received files")
	case !safeID(sessionID) || !safeID(msgID):
		return nil, errors.New("invalid session or message id")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	// Everything below root is reached through it, so a symlink planted
	// there cannot send the files elsewhere.
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rel := filepath.Join(sessionID, msgID)
	// A folder left by an attempt that did not get the message queued.
	if err := r.RemoveAll(rel); err != nil {
		return nil, err
	}
	if err := r.MkdirAll(rel, 0o700); err != nil {
		return nil, err
	}
	names := attach.DiskNames(files)
	refs := make([]fileRef, 0, len(files))
	for i, f := range files {
		if err := writeNew(r, filepath.Join(rel, names[i]), f.Data); err != nil {
			_ = r.RemoveAll(rel)
			return nil, err
		}
		refs = append(refs, fileRef{Path: filepath.Join(root, rel, names[i]), MIME: f.MIME, Size: f.Size, SHA256: f.SHA256})
	}
	return refs, nil
}

func writeNew(r *os.Root, name string, data []byte) error {
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	return errors.Join(werr, f.Close())
}

// filesLine says where a message's files are, for its header.
func filesLine(files []fileRef) string {
	if len(files) == 0 {
		return ""
	}
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = fmt.Sprintf("%s (%s, %s)", f.Path, f.MIME, attach.HumanSize(f.Size))
	}
	plural := "s"
	if len(files) == 1 {
		plural = ""
	}
	return fmt.Sprintf("(%d file%s attached: %s.)\n", len(files), plural, strings.Join(parts, ", "))
}

// filesJanitor sweeps received files hourly.
func (n *Node) filesJanitor() {
	defer n.wg.Done()
	t := time.NewTicker(filesSweep)
	defer t.Stop()
	for {
		n.pruneFiles(time.Now())
		select {
		case <-t.C:
		case <-n.closing:
			return
		}
	}
}

// pruneFiles removes the folders of messages that left the queue more than
// filesKeep ago. A queued message's folder is touched, so its age counts
// from its delivery. Only folders named like ids are touched.
func (n *Node) pruneFiles(now time.Time) {
	root := n.filesRoot()
	if root == "" {
		return
	}
	queued := map[string]bool{}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		for _, q := range s.rec.Queue {
			if len(q.Files) > 0 {
				queued[filepath.Join(s.rec.ID, q.ID)] = true
			}
		}
		s.mu.Unlock()
	}
	sessions, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, sd := range sessions {
		if !sd.IsDir() || !safeID(sd.Name()) {
			continue
		}
		msgs, err := os.ReadDir(filepath.Join(root, sd.Name()))
		if err != nil {
			continue
		}
		for _, md := range msgs {
			if !md.IsDir() || !safeID(md.Name()) {
				continue
			}
			rel := filepath.Join(sd.Name(), md.Name())
			p := filepath.Join(root, rel)
			if queued[rel] {
				_ = os.Chtimes(p, now, now)
				continue
			}
			if fi, err := md.Info(); err == nil && now.Sub(fi.ModTime()) > filesKeep {
				_ = os.RemoveAll(p)
			}
		}
		_ = os.Remove(filepath.Join(root, sd.Name())) // only if empty
	}
}
