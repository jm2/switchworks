// SPDX-License-Identifier: GPL-3.0-or-later

package acpad

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"strconv"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/acp/acptest"
	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

func testPNG(t *testing.T, w int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testImages(t *testing.T, ws ...int) []model.Attachment {
	t.Helper()
	var in []model.Attachment
	for _, w := range ws {
		in = append(in, model.Attachment{Name: "shot.png", Data: testPNG(t, w)})
	}
	out, err := attach.Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An agent that advertises promptCapabilities.image gets image content
// blocks after the text, and the transcript records only their metadata.
func TestImagesSentWhenAdvertised(t *testing.T) {
	a, state := testAdapter(t, nil)
	h, r := start(t, a, adapter.StartSpec{})
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if !reg.Caps.Images || !h.Caps().Images {
		t.Fatalf("caps %+v: the fake advertises images", reg.Caps)
	}
	imgs := testImages(t, 2, 5)
	n := r.countTurns()
	if err := h.Deliver(context.Background(), adapter.Input{Text: "look", Images: imgs}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	r.waitTurns(t, n+1)
	evs := r.all()
	msgs := items(evs, model.KindItemCompleted, "agentMessage")
	want := "echo: look\n[image image/png " + strconv.Itoa(len(imgs[0].Data)) + " bytes sha256=" + imgs[0].SHA256 + "]"
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Text, want) || !strings.Contains(msgs[0].Text, imgs[1].SHA256) {
		t.Fatalf("reply %+v, want prefix %q", msgs, want)
	}
	sess, err := acptest.ReadSession(state, h.Ref().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Images) != 2 || sess.Images[0].SHA256 != imgs[0].SHA256 || sess.Images[0].MIME != "image/png" {
		t.Fatalf("fake received %+v", sess.Images)
	}
	u := items(evs, model.KindItemStarted, "userMessage")
	if len(u) != 1 || !strings.Contains(string(u[0].Raw), `"sha256":"`+imgs[0].SHA256) || strings.Contains(string(u[0].Raw), `"data"`) {
		t.Fatalf("user item %s", u[0].Raw)
	}
}

// An agent that does not advertise images is refused before anything is
// sent: no turn starts.
func TestImagesRefusedWhenNotAdvertised(t *testing.T) {
	a, state := testAdapter(t, map[string]string{acptest.EnvNoImages: "1"})
	h, r := start(t, a, adapter.StartSpec{})
	reg := r.waitFor(t, "registered", kind(model.KindSessionRegistered)).Payload.(model.SessionRegistered)
	if reg.Caps.Images {
		t.Fatalf("caps %+v", reg.Caps)
	}
	err := h.Deliver(context.Background(), adapter.Input{Text: "look", Images: testImages(t, 2)}, adapter.DeliverQueue)
	if !errors.Is(err, adapter.ErrNoImages) || !strings.Contains(err.Error(), "promptCapabilities.image") {
		t.Fatalf("deliver: %v", err)
	}
	if r.countTurns() != 0 {
		t.Fatal("a turn started")
	}
	for _, e := range r.all() {
		if e.Kind == model.KindTurnStarted {
			t.Fatal("a turn started")
		}
	}
	// Text still works.
	if tc := r.turn(t, h, "hello"); tc.Status != "completed" {
		t.Fatalf("turn %+v", tc)
	}
	sess, _ := acptest.ReadSession(state, h.Ref().ID)
	if len(sess.Images) != 0 {
		t.Fatalf("fake received %+v", sess.Images)
	}
}
