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
//     reads them under the session's working directory only (see
//     attach.ReadWithin and docs/messaging.md).

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

// dropImages removes everything the node keeps for a stopped session.
func (n *Node) dropImages(id string) {
	_ = os.RemoveAll(n.attachDir(id))
	_ = os.RemoveAll(n.scratchDir(id))
}

// resolveImages replaces the "images" (file paths) of an agent's fleet
// request with the files' contents as "attachments". Agents cannot pass
// attachments directly: everything goes through the path checks.
func (n *Node) resolveImages(s *session, body []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body, nil // the coordinator reports bad arguments
	}
	// The coordinator matches field names without regard to case, so
	// "Attachments" counts too.
	for k := range raw {
		if strings.EqualFold(k, "attachments") {
			return nil, errors.New(`attach images by file path, in "images"`)
		}
		if strings.EqualFold(k, "images") && k != "images" {
			return nil, errors.New(`spell it "images"`)
		}
	}
	imgRaw, ok := raw["images"]
	if !ok {
		return body, nil
	}
	delete(raw, "images")
	var files []string
	if err := json.Unmarshal(imgRaw, &files); err != nil {
		return nil, errors.New(`"images" must be a list of file paths`)
	}
	if len(files) > attach.MaxImages {
		return nil, fmt.Errorf("%d images; at most %d per message", len(files), attach.MaxImages)
	}
	if len(files) > 0 {
		s.mu.Lock()
		cwd, root := s.rec.Spec.Cwd, s.rec.Spec.CwdRoot
		s.mu.Unlock()
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		if err := proc.CheckPinned(root, cwd); err != nil {
			return nil, err
		}
		atts := make([]model.Attachment, 0, len(files))
		for _, p := range files {
			// Never Switchworks's own files (node key, session secrets,
			// cli.json), even when they sit under the working directory.
			a, err := attach.ReadWithin(cwd, p, []string{n.dir, paths.Base(), filepath.Dir(paths.CLIConfig())})
			if err != nil {
				return nil, err
			}
			atts = append(atts, a)
		}
		atts, err := attach.Normalize(atts)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(atts)
		if err != nil {
			return nil, err
		}
		raw["attachments"] = b
	}
	return json.Marshal(raw)
}
