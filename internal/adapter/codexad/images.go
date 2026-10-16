// SPDX-License-Identifier: GPL-3.0-or-later

package codexad

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/attach"
	"github.com/jm2/switchyard/internal/codex"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/ulid"
)

// Images reach Codex as localImage inputs: each image is written to a
// private file (0600) in the session's scratch directory (0700, owned by
// the node), app-server reads and encodes it for the model, and the file is
// removed when the turn completes or the session stops. File names come
// from the image's digest and a fresh id, never from the sender's name.
//
// (The image variant with a data: URL would also be accepted by the
// schema, but app-server then passes the URL through unchanged and echoes
// it in the turn's userMessage item; localImage is the path the Codex TUI
// uses for pasted images, and keeps the bytes out of the event log.)

// inputs builds the turn input for in, writing its images to files.
func (h *handle) inputs(in adapter.Input) ([]codex.UserInput, []string, error) {
	input := codex.TextInput(in.Text)
	if len(in.Images) == 0 {
		return input, nil, nil
	}
	dir, err := h.scratchDir()
	if err != nil {
		return nil, nil, fmt.Errorf("image scratch directory: %w", err)
	}
	var paths []string
	for _, im := range in.Images {
		if attach.Sniff(im.Data) == "" {
			h.removeFiles(paths)
			return nil, nil, fmt.Errorf("image %s is not an allowed image type", im.Name)
		}
		p := filepath.Join(dir, fmt.Sprintf("%.16s-%s%s", attach.SHA256(im.Data), strings.ToLower(ulid.New()), attach.Ext(attach.Sniff(im.Data))))
		if err := writePrivate(p, im.Data); err != nil {
			h.removeFiles(paths)
			return nil, nil, fmt.Errorf("write image for codex: %w", err)
		}
		paths = append(paths, p)
		input = append(input, codex.LocalImageInput(p))
		h.mu.Lock()
		h.imageMeta[p] = model.Attachment{Name: im.Name, MIME: im.MIME, Size: im.Size, SHA256: im.SHA256}
		h.mu.Unlock()
	}
	return input, paths, nil
}

func writePrivate(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(p)
		return err
	}
	return f.Close()
}

// scratchDir returns the session's directory for image files, creating a
// private temporary one when the node gave none (swyd dev run).
func (h *handle) scratchDir() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scratch != "" {
		return h.scratch, os.MkdirAll(h.scratch, 0o700)
	}
	d, err := os.MkdirTemp("", "swyd-codex-images-")
	if err != nil {
		return "", err
	}
	h.scratch, h.ownScratch = d, true
	return d, nil
}

// adoptFiles records a turn's image files, or removes them at once when
// the input was not accepted or the turn has already completed.
func (h *handle) adoptFiles(turnID string, paths []string, err error) {
	if len(paths) == 0 {
		return
	}
	h.mu.Lock()
	if err != nil || turnID == "" || containsStr(h.doneTurns, turnID) {
		h.mu.Unlock()
		h.removeFiles(paths)
		return
	}
	h.files[turnID] = append(h.files[turnID], paths...)
	h.mu.Unlock()
}

func (h *handle) removeFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
	if len(paths) == 0 {
		return
	}
	h.mu.Lock()
	for _, p := range paths {
		delete(h.imageMeta, p)
	}
	h.mu.Unlock()
}

// cleanImages removes every image file the session still holds.
func (h *handle) cleanImages() {
	h.mu.Lock()
	var all []string
	for id, ps := range h.files {
		all = append(all, ps...)
		delete(h.files, id)
	}
	dir, own := h.scratch, h.ownScratch
	if own {
		h.scratch, h.ownScratch = "", false
	}
	h.mu.Unlock()
	h.removeFiles(all)
	if own && dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// userItem rewrites a userMessage item's content for the event log:
// localImage paths Switchyard wrote become the image's metadata, and data:
// URLs are dropped, so neither scratch paths nor image bytes are stored.
func (h *handle) userItem(raw json.RawMessage) json.RawMessage {
	var item map[string]any
	if json.Unmarshal(raw, &item) != nil {
		return raw
	}
	content, ok := item["content"].([]any)
	if !ok {
		return raw
	}
	changed := false
	h.mu.Lock()
	for i, c := range content {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "localImage":
			p, _ := m["path"].(string)
			if meta, ok := h.imageMeta[p]; ok {
				content[i] = map[string]any{"type": "image", "name": meta.Name, "mime": meta.MIME, "size": meta.Size, "sha256": meta.SHA256}
				changed = true
			}
		case "image":
			if u, _ := m["url"].(string); strings.HasPrefix(u, "data:") {
				m["url"] = "data:(omitted by Switchyard)"
				changed = true
			}
		}
	}
	h.mu.Unlock()
	if !changed {
		return raw
	}
	item["content"] = content
	b, err := json.Marshal(item)
	if err != nil {
		return raw
	}
	return b
}
