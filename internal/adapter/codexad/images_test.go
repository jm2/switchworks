// SPDX-License-Identifier: GPL-3.0-or-later

package codexad

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

func testImage(t *testing.T, w int) model.Attachment {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 2))); err != nil {
		t.Fatal(err)
	}
	out, err := attach.Normalize([]model.Attachment{{Name: "../../etc/shot.png", Data: buf.Bytes()}})
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}

// Images reach app-server as localImage files in the session's scratch
// directory: private, named by digest (never by the sender's name), read
// by app-server when the turn starts, and removed when the turn completes.
func TestLocalImageInputAndCleanup(t *testing.T) {
	a := newFake(t, t.TempDir())
	ctx := context.Background()
	col := &collector{}
	scratch := filepath.Join(t.TempDir(), "scratch", "s1")
	h, err := a.Start(ctx, adapter.StartSpec{Name: "devbox/img", Cwd: t.TempDir(), ScratchDir: scratch}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Caps().Images {
		t.Fatal("codex sessions take images")
	}
	col.waitFor(t, 0, "idle", status(model.StatusIdle))
	img := testImage(t, 3)
	if err := h.Deliver(ctx, adapter.Input{Text: "what is this?", Images: []model.Attachment{img}}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "user item", func(e adapter.Event) bool {
		it, ok := e.Payload.(model.Item)
		return ok && it.ItemType == "userMessage"
	})
	user := col.snapshot()[i].Payload.(model.Item)
	raw := string(user.Raw)
	// The item names the image by its metadata, not by the scratch path.
	if !strings.Contains(raw, img.SHA256) || strings.Contains(raw, scratch) || strings.Contains(raw, "localImage") {
		t.Fatalf("user item %s", raw)
	}
	col.waitFor(t, i, "reply with the image", completedText("sha256="+img.SHA256))
	col.waitFor(t, i, "turn.completed", kind(model.KindTurnCompleted))
	// The file is gone once the turn has completed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(scratch)
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scratch files left: %v", entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(scratch)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("scratch dir %v %v", fi, err)
		}
	}
}

// The localImage input is what the committed schema accepts, and the
// files are written 0600 under the scratch directory.
func TestInputsWritePrivateFiles(t *testing.T) {
	scratch := t.TempDir()
	h := &handle{scratch: scratch, files: map[string][]string{}, imageMeta: map[string]model.Attachment{}}
	img := testImage(t, 4)
	in, paths, err := h.inputs(adapter.Input{Text: "x", Images: []model.Attachment{img}})
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 || in[0].Type != "text" || in[1].Type != "localImage" || len(paths) != 1 || in[1].Path != paths[0] {
		t.Fatalf("inputs %+v %v", in, paths)
	}
	b, _ := json.Marshal(in)
	if string(b) != `[{"type":"text","text":"x"},{"type":"localImage","path":`+mustJSON(paths[0])+`}]` {
		t.Fatalf("wire form %s", b)
	}
	if filepath.Dir(paths[0]) != scratch || !strings.HasPrefix(filepath.Base(paths[0]), img.SHA256[:16]+"-") || strings.Contains(paths[0], "etc") {
		t.Fatalf("path %s", paths[0])
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(paths[0])
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	got, _ := os.ReadFile(paths[0])
	if !bytes.Equal(got, img.Data) {
		t.Fatal("file content differs")
	}
	// Not accepted: removed at once.
	h.adoptFiles("", paths, context.Canceled)
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("file kept after a failed start: %v", err)
	}
	// No scratch directory given: a private temporary one, removed with
	// the session.
	h2 := &handle{files: map[string][]string{}, imageMeta: map[string]model.Attachment{}}
	_, paths2, err := h2.inputs(adapter.Input{Text: "x", Images: []model.Attachment{img}})
	if err != nil {
		t.Fatal(err)
	}
	h2.adoptFiles("turn-1", paths2, nil)
	dir := filepath.Dir(paths2[0])
	h2.cleanImages()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp scratch dir kept: %v", err)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
