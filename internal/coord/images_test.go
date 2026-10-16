// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
)

func testPNG(t *testing.T, w int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRouterImages(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	_ = s.store.UpsertHost(model.Host{ID: "devbox", Images: []string{"codex", "goose"}})
	_ = s.store.UpsertHost(model.Host{ID: "oldbox"})
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	old := addSession(t, s, "oldbox", "c")
	agy := addSession(t, s, "devbox", "agy", func(x *model.Session) { x.Harness = "agy" })
	gooseNo := addSession(t, s, "devbox", "goose", func(x *model.Session) {
		x.Harness = "goose"
		x.NativeRef = &model.NativeRef{Harness: "goose", ID: "g1"}
		x.Caps = model.Caps{Message: true}
	})
	img := testPNG(t, 3)
	sum := attach.SHA256(img)
	att := []model.Attachment{{Name: "/tmp/../shot.png", Data: img}}

	msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "devbox/b", Text: "look", Attachments: att})
	if err != nil {
		t.Fatal(err)
	}
	m := msgs[0]
	if m.State != model.MsgPending || len(m.Attachments) != 1 || m.Attachments[0].Data != nil ||
		m.Attachments[0].SHA256 != sum || m.Attachments[0].Name != "shot.png" || m.Attachments[0].MIME != "image/png" {
		t.Fatalf("message %+v", m)
	}
	stored, _ := s.store.Message(m.ID)
	if b, _ := json.Marshal(stored); bytes.Contains(b, []byte(`"data"`)) {
		t.Fatalf("stored message carries data: %s", b)
	}
	if !s.blobs.has(sum) {
		t.Fatal("image bytes not kept for delivery")
	}
	full, err := s.loadAttachments(stored.Attachments)
	if err != nil || !bytes.Equal(full[0].Data, img) {
		t.Fatalf("load: %v", err)
	}

	refused := func(to, want string, args fleet.SendArgs) {
		t.Helper()
		args.To, args.Text = to, "x"
		if args.Attachments == nil && args.Images == nil {
			args.Attachments = att
		}
		_, err := r.send(ctx, from(a), model.MsgNotify, args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("send to %s: %v, want %q", to, err, want)
		}
	}
	refused(old.Path, "older swk node", fleet.SendArgs{})
	refused(agy.Path, "text only", fleet.SendArgs{})
	refused(gooseNo.Path, "does not advertise image prompts", fleet.SendArgs{})
	refused(b.Path, "older swk node", fleet.SendArgs{Images: []string{"shot.png"}}) // paths reaching the coordinator
	refused(b.Path, "not a PNG", fleet.SendArgs{Attachments: []model.Attachment{{Data: []byte("<svg/>")}}})
	five := []model.Attachment{{Data: img}, {Data: img}, {Data: img}, {Data: img}, {Data: img}}
	refused(b.Path, "at most 4", fleet.SendArgs{Attachments: five})

	// People may receive images (they see them in the web UI).
	um, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "user:dev", Text: "fyi", Attachments: att})
	if err != nil || um[0].State != model.MsgDelivered || um[0].Attachments[0].SHA256 != sum {
		t.Fatalf("to a person: %+v %v", um, err)
	}
	// An ask with an image.
	res := r.ask(ctx, from(a), fleet.AskArgs{To: "devbox/b", Text: "ok?", WaitSeconds: new(int), Attachments: att})
	if res.Status == fleet.AskFailed {
		t.Fatalf("ask: %+v", res)
	}
	if ask, _ := s.store.Message(res.MessageID); len(ask.Attachments) != 1 {
		t.Fatalf("ask %+v", ask)
	}

	// A missing blob fails delivery rather than sending text alone.
	if err := os.Remove(filepath.Join(s.blobs.dir, sum)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadAttachments(stored.Attachments); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("missing blob: %v", err)
	}
}

func TestBlobStoreRetention(t *testing.T) {
	b, err := openBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p1, p2, p3 := testPNG(t, 1), testPNG(t, 2), testPNG(t, 3)
	s1, _ := b.put(p1)
	s2, _ := b.put(p2)
	s3, _ := b.put(p3)
	old := time.Now().Add(-blobKeep - time.Hour)
	for _, s := range []string{s1, s2} {
		_ = os.Chtimes(filepath.Join(b.dir, s), old, old)
	}
	b.prune(time.Now(), map[string]bool{s2: true}) // s2 belongs to a pending message
	if b.has(s1) || !b.has(s2) || !b.has(s3) {
		t.Fatalf("after prune: %v %v %v", b.has(s1), b.has(s2), b.has(s3))
	}
	// Names are digests only.
	if _, err := b.get("../coordinator.json"); err == nil {
		t.Fatal("path accepted as an attachment id")
	}
	// Corruption is detected.
	_ = os.WriteFile(filepath.Join(b.dir, s3), []byte("tampered"), 0o600)
	if _, err := b.get(s3); err == nil {
		t.Fatal("corrupt blob served")
	}
	// The hard cap refuses new images (undelivered ones fill it).
	b.total = blobHardMax
	if _, err := b.put(testPNG(t, 9)); !errors.Is(err, errBlobStoreFull) {
		t.Fatalf("put over the cap: %v", err)
	}
	if _, err := b.put(p2); err != nil {
		t.Fatalf("an image already stored is accepted again: %v", err)
	}
	if runtimeMode(t, b.dir) != 0o700 {
		t.Fatal("attachment directory is not private")
	}
}

func runtimeMode(t *testing.T, p string) os.FileMode {
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '\\' {
		return 0o700
	}
	return fi.Mode().Perm()
}

// The endpoints that take images accept bodies up to maxImageBody; the
// others keep the 4 MiB limit.
func TestImageBodyLimits(t *testing.T) {
	big := `{"text":"` + strings.Repeat("a", 5<<20) + `"}`
	var v struct {
		Text string `json:"text"`
	}
	if err := decodeBody(httptest.NewRequest("POST", "/", strings.NewReader(big)), &v); err == nil {
		t.Fatal("5 MiB accepted by the default limit")
	}
	if err := decodeBodyLimit(httptest.NewRequest("POST", "/", strings.NewReader(big)), &v, maxImageBody); err != nil {
		t.Fatalf("5 MiB refused by the image limit: %v", err)
	}
	huge := `{"text":"` + strings.Repeat("a", maxImageBody) + `"}`
	if err := decodeBodyLimit(httptest.NewRequest("POST", "/", strings.NewReader(huge)), &v, maxImageBody); err == nil {
		t.Fatal("over the image limit accepted")
	}
	// The largest message (10 MiB of images as base64, plus text) fits.
	if 4*(attach.MaxTotalBytes+2)/3+fleet.MaxTextBytes+64<<10 > maxImageBody {
		t.Fatal("maxImageBody is too small for the largest message")
	}
}
