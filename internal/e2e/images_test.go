// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte((i*2654435761)>>13 ^ i*31)
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestFleetImages is the screenshot gate: an agent on one host sends a
// screenshot file with fleet_send, and the reviewer's harness on another
// host receives exactly those bytes. Paths outside the sender's working
// directory are refused; events and stored messages carry only metadata.
func TestFleetImages(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer co.stop()
	c := apiClient(t, coordDir)
	_, _, ndA := enrollHost(t, c, "devbox")
	defer ndA.stop(t)
	_, _, ndB := enrollHost(t, c, "buildbox")
	defer ndB.stop(t)
	waitOnline(t, c, "devbox")
	waitOnline(t, c, "buildbox")

	hosts, _ := c.Hosts(ctx)
	for _, h := range hosts {
		if len(h.Images) != 1 || h.Images[0] != "codex" {
			t.Fatalf("host %s images %v", h.ID, h.Images)
		}
	}

	workA, workB := t.TempDir(), t.TempDir()
	shot := pngImage(t, 64, 40)
	sum := attach.SHA256(shot)
	if err := os.MkdirAll(filepath.Join(workA, "shots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workA, "shots", "level1.png"), shot, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(secret, pngImage(t, 3, 3), 0o600); err != nil {
		t.Fatal(err)
	}

	game, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "game", Cwd: workA})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := c.Spawn(ctx, model.SpawnSpec{Host: "buildbox", Name: "reviewer", Cwd: workB})
	if err != nil {
		t.Fatal(err)
	}
	wg := watch(c, game.ID)
	defer wg.cancel()
	wr := watch(c, reviewer.ID)
	defer wr.cancel()
	waitStatus(t, c, game.ID, model.StatusIdle)
	waitStatus(t, c, reviewer.ID, model.StatusIdle)
	say := func(id, text string) {
		t.Helper()
		if _, err := c.Send(ctx, id, text, "queue", ""); err != nil {
			t.Fatal(err)
		}
	}

	// The game agent sends its screenshot, by a path relative to its cwd.
	say(game.ID, call("fleet_send", map[string]any{"to": "buildbox/reviewer", "text": "does level 1 render?", "images": []string{"shots/level1.png"}}))
	i := wg.waitFor(t, "sent", 0, agentSaidAll("tool fleet_send:", `"state": "delivered"`))
	j := wr.waitFor(t, "reviewer saw the image", 0, agentSaidAll("from=devbox/game", "1 image attached", "level1.png",
		"[image image/png", "sha256="+sum, "does level 1 render?"))
	wr.waitFor(t, "reviewer turn done", j, turnEnded("completed"))

	// Outside the working directory: refused, nothing sent.
	say(game.ID, call("fleet_send", map[string]any{"to": "buildbox/reviewer", "text": "x", "images": []string{secret}}))
	i = wg.waitFor(t, "outside refused", i+1, agentSaidAll("tool fleet_send: Error:", "outside this session's working directory"))
	say(game.ID, call("fleet_send", map[string]any{"to": "buildbox/reviewer", "text": "x", "images": []string{"../" + filepath.Base(filepath.Dir(secret)) + "/secret.png"}}))
	i = wg.waitFor(t, "relative escape refused", i+1, agentSaidAll("tool fleet_send: Error:", "outside"))
	// Image data cannot be passed directly by an agent.
	say(game.ID, call("fleet_send", map[string]any{"to": "buildbox/reviewer", "text": "x",
		"attachments": []map[string]any{{"name": "x.png", "data": base64.StdEncoding.EncodeToString(shot)}}}))
	i = wg.waitFor(t, "inline data refused", i+1, agentSaidAll("tool fleet_send: Error:", "by file path"))
	// Not an image.
	if err := os.WriteFile(filepath.Join(workA, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	say(game.ID, call("fleet_send", map[string]any{"to": "buildbox/reviewer", "text": "x", "images": []string{"notes.txt"}}))
	wg.waitFor(t, "text file refused", i+1, agentSaidAll("tool fleet_send: Error:", "not a PNG"))

	// The stored message and the events have metadata only.
	msgs, err := c.Messages(ctx, apiclient.MessageQuery{Session: reviewer.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var withImage *model.Message
	for k := range msgs {
		if len(msgs[k].Attachments) > 0 {
			withImage = &msgs[k]
		}
	}
	if withImage == nil || withImage.Attachments[0].SHA256 != sum || withImage.Attachments[0].Data != nil ||
		withImage.Attachments[0].Name != "level1.png" || withImage.Attachments[0].MIME != "image/png" {
		t.Fatalf("stored message %+v", withImage)
	}
	b64 := base64.StdEncoding.EncodeToString(shot)
	sawMeta := false
	wr.mu.Lock()
	for _, e := range wr.evs {
		if strings.Contains(string(e.Payload), b64[:min(200, len(b64))]) {
			t.Errorf("event %s %s carries the image bytes", e.Kind, e.Payload)
		}
		if e.Kind == model.KindMessageDelivered && strings.Contains(string(e.Payload), sum) {
			sawMeta = true
		}
	}
	wr.mu.Unlock()
	if !sawMeta {
		t.Fatal("no message.delivered event with the image's metadata")
	}
	// The coordinator serves the image for the web UI.
	got, err := c.Attachment(ctx, sum)
	if err != nil || !bytes.Equal(got.Data, shot) || got.MIME != "image/png" {
		t.Fatalf("attachment: %v %+v", err, got.MIME)
	}
	if _, err := c.Attachment(ctx, strings.Repeat("0", 64)); err == nil {
		t.Fatal("unknown attachment served")
	}
	if _, err := c.Attachment(ctx, "..%2f..%2fcoordinator.json"); err == nil {
		t.Fatal("bad attachment id accepted")
	}

	// A person sends an image (swk send --image), and spawns with one.
	other := pngImage(t, 10, 10)
	k := wr.len()
	if _, err := c.Send(ctx, reviewer.ID, "and this one?", "queue", "", model.Attachment{Name: "two.png", Data: other}); err != nil {
		t.Fatal(err)
	}
	wr.waitFor(t, "person's image", k, agentSaidAll("echo: and this one?", "sha256="+attach.SHA256(other)))
	spawned, err := c.Spawn(ctx, model.SpawnSpec{Host: "buildbox", Name: "first-look", Cwd: workB, Prompt: "first look",
		Attachments: []model.Attachment{{Name: "three.png", Data: shot}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := watch(c, spawned.ID)
	defer ws.cancel()
	ws.waitFor(t, "spawn prompt image", 0, agentSaidAll("echo: first look", "sha256="+sum))

	// A declared type that does not match the bytes, and a non-image.
	if _, err := c.Send(ctx, reviewer.ID, "x", "queue", "", model.Attachment{Name: "x.png", MIME: "image/jpeg", Data: shot}); err == nil {
		t.Fatal("mismatched type accepted")
	}
	if _, err := c.Send(ctx, reviewer.ID, "x", "queue", "", model.Attachment{Name: "x.svg", Data: []byte("<svg/>")}); err == nil {
		t.Fatal("svg accepted")
	}

	// An agent's ask with an image, answered by the other agent.
	say(game.ID, call("fleet_ask", map[string]any{"to": "buildbox/reviewer", "text": "approve this frame?", "wait_seconds": 30,
		"images": []string{filepath.Join(workA, "shots", "level1.png")}}))
	wg.waitFor(t, "ask answered", i+1, agentSaidAll("tool fleet_ask: Answer from buildbox/reviewer:", "answer: approve this frame?"))

	// fleet_spawn with an image: the child's first prompt carries it.
	say(game.ID, call("fleet_spawn", map[string]any{"prompt": "check the frame", "name": "frame-check", "images": []string{"shots/level1.png"}}))
	wg.waitFor(t, "spawned", i+1, agentSaidAll("tool fleet_spawn:", "devbox/frame-check"))
	child, err := c.Resolve(ctx, "devbox/frame-check")
	if err != nil {
		t.Fatal(err)
	}
	wc := watch(c, child.ID)
	defer wc.cancel()
	wc.waitFor(t, "child saw the image", 0, agentSaidAll("check the frame", "sha256="+sum))
}
