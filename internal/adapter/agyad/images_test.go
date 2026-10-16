// SPDX-License-Identifier: GPL-3.0-or-later

package agyad

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/model"
)

// agy's headless stream-json input documents text blocks only, so images
// are refused before anything is written to agy.
func TestImagesRefused(t *testing.T) {
	a, log := newAdapter(t, Options{})
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if h.Caps().Images {
		t.Fatal("agy sessions must not claim images")
	}
	png := []byte("\x89PNG\r\n\x1a\n0000")
	err = h.Deliver(ctx, adapter.Input{Text: "look", Images: []model.Attachment{{Name: "x.png", MIME: "image/png", Data: png}}}, adapter.DeliverQueue)
	if !errors.Is(err, adapter.ErrNoImages) || !strings.Contains(err.Error(), "text") {
		t.Fatalf("deliver: %v", err)
	}
	if len(invocations(t, log)) != 0 {
		t.Fatal("agy was started for a refused prompt")
	}
	col.turn(t, h, "hi", "completed") // text still works
}
