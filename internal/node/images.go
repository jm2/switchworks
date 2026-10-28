// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/codex"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/proc"
)

// Image attachments on the node:
//
//   - A queued message keeps only its images' metadata in state.json; the
//     bytes wait in attachments/<session>/<sha256> (0600, directory 0700)
//     until the message is delivered, then are removed. File names are the
//     digest the node computed, never a name a sender chose.
//   - scratch/<session> is the directory adapters may write into for the
//     harness (Codex's localImage files). It is emptied when the session
//     resumes (no turn survives a node restart) and removed when it stops.
//   - Agents attach images by path through the fleet tools; resolveImages
//     reads them under the session's working directory only, never from
//     the places attachPolicy denies (see attach.ReadWithin and
//     docs/messaging.md).

func (n *Node) attachDir(id string) string  { return filepath.Join(n.dir, "attachments", id) }
func (n *Node) scratchDir(id string) string { return filepath.Join(n.dir, "scratch", id) }

// noImages explains why a session cannot take images.
func noImages(harnessName string) error {
	why := "an ACP agent must advertise promptCapabilities.image"
	if harnessName == "agy" {
		why = "agy's headless input is text only"
	}
	return fmt.Errorf("%w: %s sessions cannot take images here (%s)", adapter.ErrNoImages, harnessName, why)
}

// storeImages writes normalized images for a session's queue and returns
// their metadata.
func (n *Node) storeImages(id string, images []model.Attachment) ([]model.Attachment, error) {
	if len(images) == 0 {
		return nil, nil
	}
	dir := n.attachDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, im := range images {
		if !attach.ValidSHA256(im.SHA256) {
			return nil, errors.New("image without a valid digest")
		}
		p := filepath.Join(dir, im.SHA256)
		if old, err := os.ReadFile(p); err == nil && attach.SHA256(old) == im.SHA256 {
			continue // a torn file (crash before its data was on disk) is rewritten
		}
		tmp := p + ".tmp"
		if err := paths.WriteFile(tmp, im.Data, 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
	}
	return model.MetaOnly(images), nil
}

// loadImages reads a queued message's images back, checking each against
// its digest.
func (n *Node) loadImages(id string, metas []model.Attachment) ([]model.Attachment, error) {
	if len(metas) == 0 {
		return nil, nil
	}
	out := make([]model.Attachment, 0, len(metas))
	for _, m := range metas {
		if !attach.ValidSHA256(m.SHA256) {
			return nil, errors.New("queued image without a valid digest")
		}
		b, err := os.ReadFile(filepath.Join(n.attachDir(id), m.SHA256))
		if err != nil {
			return nil, fmt.Errorf("queued image %s is missing: %w", m.Name, err)
		}
		if attach.SHA256(b) != m.SHA256 {
			return nil, fmt.Errorf("queued image %s is corrupt", m.Name)
		}
		m.Data = b
		out = append(out, m)
	}
	return attach.Normalize(out)
}

// gcImages removes stored images no queued message refers to any more.
func (n *Node) gcImages(s *session) {
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	s.mu.Lock()
	keep := map[string]bool{}
	for _, q := range s.rec.Queue {
		for _, a := range q.Attachments {
			keep[a.SHA256] = true
		}
	}
	id := s.rec.ID
	s.mu.Unlock()
	entries, err := os.ReadDir(n.attachDir(id))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			_ = os.Remove(filepath.Join(n.attachDir(id), e.Name()))
		}
	}
}

// dropSessionDirs removes everything the node keeps for a stopped session:
// its images, scratch and cache directories.
func (n *Node) dropSessionDirs(id string) {
	_ = os.RemoveAll(n.attachDir(id))
	_ = os.RemoveAll(n.scratchDir(id))
	n.dropCache(id)
}

// pathArg is a fleet argument that lists files an agent attaches by path,
// and the argument the node replaces it with, holding their contents.
type pathArg struct {
	paths, data string // e.g. "images" -> "attachments"
	max         int
	read        func(dir, name string, p attach.Policy) (model.Attachment, error)
	normalize   func([]model.Attachment) ([]model.Attachment, error)
}

var (
	imagesArg = pathArg{"images", "attachments", attach.MaxImages, attach.ReadWithin, attach.Normalize}
	filesArg  = pathArg{"files", "file_attachments", attach.MaxFiles, attach.ReadFileWithin, attach.NormalizeFiles}
)

// resolveImages replaces the "images" (file paths) of an agent's fleet
// request with the files' contents as "attachments", and likewise each
// other pathArg given ("files" with "file_attachments"). Agents cannot
// pass the contents directly: everything goes through the path checks.
func (n *Node) resolveImages(s *session, body []byte, extra ...pathArg) ([]byte, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body, nil // the coordinator reports bad arguments
	}
	args := append([]pathArg{imagesArg}, extra...)
	// The coordinator matches field names without regard to case, so
	// "Attachments" counts too.
	for k := range raw {
		for _, a := range args {
			if strings.EqualFold(k, a.data) {
				return nil, fmt.Errorf("attach %s by file path, in %q", a.paths, a.paths)
			}
			if strings.EqualFold(k, a.paths) && k != a.paths {
				return nil, fmt.Errorf("spell it %q", a.paths)
			}
		}
	}
	changed := false
	for _, a := range args {
		listRaw, ok := raw[a.paths]
		if !ok {
			continue
		}
		delete(raw, a.paths)
		changed = true
		b, err := n.readPaths(s, a, listRaw)
		if err != nil {
			return nil, err
		}
		if b != nil {
			raw[a.data] = b
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(raw)
}

// readPaths reads the files of one pathArg under the session's working
// directory and returns their contents, as JSON, or nil for none.
func (n *Node) readPaths(s *session, a pathArg, listRaw json.RawMessage) (json.RawMessage, error) {
	var files []string
	if err := json.Unmarshal(listRaw, &files); err != nil {
		return nil, fmt.Errorf("%q must be a list of file paths", a.paths)
	}
	if len(files) > a.max {
		return nil, fmt.Errorf("%d %s; at most %d per message", len(files), a.paths, a.max)
	}
	if len(files) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	cwd, root := s.rec.Spec.Cwd, s.rec.Spec.CwdRoot
	s.mu.Unlock()
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	if err := proc.CheckPinned(root, cwd); err != nil {
		return nil, err
	}
	pol := n.attachPolicy()
	atts := make([]model.Attachment, 0, len(files))
	for _, p := range files {
		at, err := a.read(cwd, p, pol)
		if err != nil {
			return nil, err
		}
		atts = append(atts, at)
	}
	atts, err := a.normalize(atts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(atts)
}

// attachPolicy is what agents may not attach on this node, whatever their
// working directory. The node reads attachments as the user, outside any
// sandbox, so it denies what a sandbox profile would: Switchworks's own
// files (node key, session secrets, cli.json); the credential locations
// of attach.CredentialPaths; CODEX_HOME; the received files of every
// session; each path a permission profile in Codex's config.toml denies;
// and attach_deny. It is built for each request, so an edit to
// config.toml counts at once.
func (n *Node) attachPolicy() attach.Policy {
	home, _ := os.UserHomeDir()
	p := attach.Policy{Own: []string{n.dir, paths.Base(), filepath.Dir(paths.CLIConfig())}, Home: home}
	p.Deny = append(attach.CredentialPaths(home), n.filesRoot())
	if ch := codexHome(n.cfg); ch != "" {
		p.Deny = append(p.Deny, ch)
		toml, _ := os.ReadFile(filepath.Join(ch, "config.toml"))
		for _, d := range codex.ProfileDenyPaths(toml) {
			if d = attach.ExpandHome(d, home); filepath.IsAbs(d) {
				p.Deny = append(p.Deny, d)
			}
		}
	}
	for _, d := range n.cfg.AttachDeny {
		if d = attach.ExpandHome(d, home); filepath.IsAbs(d) {
			p.Deny = append(p.Deny, d)
		}
	}
	for _, a := range n.cfg.AttachAllow {
		if a = attach.ExpandHome(a, home); filepath.IsAbs(a) {
			p.Allow = append(p.Allow, a)
		}
	}
	return p
}

// checkAttachPaths refuses attach_deny and attach_allow entries that are
// neither absolute nor under "~/": relative to what would be a guess.
func checkAttachPaths(cfg Config) error {
	for key, list := range map[string][]string{"attach_deny": cfg.AttachDeny, "attach_allow": cfg.AttachAllow} {
		for _, p := range list {
			if !filepath.IsAbs(p) && p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
				return fmt.Errorf("node.json %s: %q is neither absolute nor under ~/", key, p)
			}
		}
	}
	return nil
}
