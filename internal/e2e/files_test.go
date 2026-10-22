// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

// formatPatch is a git format-patch of about n bytes.
func formatPatch(n int) []byte {
	var b bytes.Buffer
	b.WriteString("From 3f1c2a9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c Mon Sep 17 00:00:00 2001\nFrom: Dev <dev@example.com>\nSubject: [PATCH] Add the parser\n\n---\ndiff --git a/parser.go b/parser.go\n--- a/parser.go\n+++ b/parser.go\n@@ -1,1 +1,9999 @@\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "+\tx%d := \"ünïcode %d\" // keep me byte for byte\r\n", i, i)
	}
	b.WriteString("-- \n2.47.0\n")
	return b.Bytes()
}

// TestFleetFiles is the patch gate: an agent on one host sends a 200 KB
// format-patch with fleet_send, and the receiving node writes exactly
// those bytes to a folder of its own, named in the message, so the
// receiver can apply it without the model copying it.
func TestFleetFiles(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer co.stop()
	c := apiClient(t, coordDir)
	_, _, ndA := enrollHost(t, c, "devbox")
	defer ndA.stop(t)
	dirB, _, ndB := enrollHost(t, c, "buildbox")
	defer ndB.stop(t)
	waitOnline(t, c, "devbox")
	waitOnline(t, c, "buildbox")

	hosts, _ := c.Hosts(ctx)
	for _, h := range hosts {
		if !h.Files {
			t.Fatalf("host %s does not offer files", h.ID)
		}
	}

	workA, workB := t.TempDir(), t.TempDir()
	patch := formatPatch(200 << 10)
	if err := os.WriteFile(filepath.Join(workA, "0001-parser.patch"), patch, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workA, "blob.bin"), []byte("PK\x03\x04\x00\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	game, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "win", Cwd: workA})
	if err != nil {
		t.Fatal(err)
	}
	linux, err := c.Spawn(ctx, model.SpawnSpec{Host: "buildbox", Name: "linux", Cwd: workB})
	if err != nil {
		t.Fatal(err)
	}
	wg := watch(c, game.ID)
	defer wg.cancel()
	wr := watch(c, linux.ID)
	defer wr.cancel()
	waitStatus(t, c, game.ID, model.StatusIdle)
	waitStatus(t, c, linux.ID, model.StatusIdle)
	say := func(text string) {
		t.Helper()
		if _, err := c.Send(ctx, game.ID, text, "queue", ""); err != nil {
			t.Fatal(err)
		}
	}

	say(call("fleet_send", map[string]any{"to": "buildbox/linux", "text": "please git am this", "files": []string{"0001-parser.patch"}}))
	i := wg.waitFor(t, "sent", 0, agentSaidAll("tool fleet_send:", `"state": "delivered"`))

	msgs, err := c.Messages(ctx, apiclient.MessageQuery{Session: linux.ID, Limit: 10})
	if err != nil || len(msgs) != 1 || len(msgs[0].Files) != 1 {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
	m := msgs[0]
	if f := m.Files[0]; f.Name != "0001-parser.patch" || f.MIME != "text/x-patch" || f.Size != int64(len(patch)) ||
		f.SHA256 != attach.SHA256(patch) || f.Data != nil {
		t.Fatalf("stored file %+v", f)
	}
	path := filepath.Join(dirB, "received-files", linux.ID, m.ID, "0001-parser.patch")
	j := wr.waitFor(t, "receiver saw the path", 0, agentSaidAll("from=devbox/win",
		fmt.Sprintf("(1 file attached: %s (text/x-patch, %s).)", path, attach.HumanSize(int64(len(patch)))), "please git am this"))
	wr.waitFor(t, "receiver turn done", j, turnEnded("completed"))
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, patch) {
		t.Fatalf("received file differs (%d bytes, %v)", len(got), err)
	}
	if fi, err := os.Stat(path); err != nil || (fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows") {
		t.Fatalf("received file mode: %v %v", fi.Mode(), err)
	}

	// Outside the working directory, not text, or data passed directly:
	// refused, nothing sent.
	say(call("fleet_send", map[string]any{"to": "buildbox/linux", "text": "x", "files": []string{secret}}))
	i = wg.waitFor(t, "outside refused", i+1, agentSaidAll("tool fleet_send: Error:", "outside this session's working directory"))
	say(call("fleet_send", map[string]any{"to": "buildbox/linux", "text": "x", "files": []string{"blob.bin"}}))
	i = wg.waitFor(t, "binary refused", i+1, agentSaidAll("tool fleet_send: Error:", "not a text file"))
	say(call("fleet_send", map[string]any{"to": "buildbox/linux", "text": "x",
		"file_attachments": []map[string]any{{"name": "x.txt", "data": base64.StdEncoding.EncodeToString([]byte("hi"))}}}))
	i = wg.waitFor(t, "inline data refused", i+1, agentSaidAll("tool fleet_send: Error:", "by file path"))
	// To a person: refused.
	say(call("fleet_send", map[string]any{"to": "user:jm", "text": "x", "files": []string{"0001-parser.patch"}}))
	wg.waitFor(t, "person refused", i+1, agentSaidAll("tool fleet_send: Error:", "agent sessions only"))
	if msgs, _ := c.Messages(ctx, apiclient.MessageQuery{Session: linux.ID, Limit: 10}); len(msgs) != 1 {
		t.Fatalf("refused sends stored: %d messages", len(msgs))
	}

	// A person sends a file (swk send --file); events never carry the
	// bytes.
	k := wr.len()
	if _, err := c.SendMessage(ctx, linux.ID, apiclient.SessionMessage{Text: "and the log", Mode: "queue",
		Files: []model.Attachment{{Name: "build.log", Data: []byte("ok\n")}}}); err != nil {
		t.Fatal(err)
	}
	wr.waitFor(t, "person's file", k, agentSaidAll("1 file attached: ", "build.log (text/plain, 3 B)", "and the log"))
	b64 := base64.StdEncoding.EncodeToString(patch[:3000])
	wr.mu.Lock()
	for _, e := range wr.evs {
		if strings.Contains(string(e.Payload), b64[:200]) {
			t.Errorf("event %s carries the file's bytes", e.Kind)
		}
	}
	wr.mu.Unlock()
}
