// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/wire"
)

// Files go through the router like images: checked, kept in the
// attachment store by digest, recorded as metadata, and delivered with
// their bytes, only to nodes that take them.
func TestRouterFiles(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{})
	r := s.router
	var mu sync.Mutex
	var got []wire.MessageCmd
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		if cmd.Op == wire.OpDeliver {
			mu.Lock()
			got = append(got, *cmd.Message)
			mu.Unlock()
		}
		return wire.Result{OK: true}
	})
	s.mu.Lock()
	s.nodes["devbox"].hello.Features = []string{wire.FeatureFiles}
	s.mu.Unlock()
	_ = s.store.UpsertHost(model.Host{ID: "oldbox"})
	a := addSession(t, s, "devbox", "a")
	addSession(t, s, "devbox", "b")
	agy := addSession(t, s, "devbox", "agy", func(x *model.Session) { x.Harness = "agy" })
	old := addSession(t, s, "oldbox", "c")

	patch := []byte("From 0123 Mon Sep 17 00:00:00 2001\nSubject: [PATCH] x\n\ndiff --git a/x b/x\n")
	sum := attach.SHA256(patch)
	files := []model.Attachment{{Name: "/home/me/0001-x.patch", Data: patch}}
	msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: "b", Text: "git am this", FileAttachments: files})
	if err != nil {
		t.Fatal(err)
	}
	m := msgs[0]
	if m.State != model.MsgDelivered || len(m.Files) != 1 || m.Files[0].Data != nil || m.Files[0].Name != "0001-x.patch" ||
		m.Files[0].MIME != "text/x-patch" || m.Files[0].SHA256 != sum || len(m.Attachments) != 0 {
		t.Fatalf("message %+v", m)
	}
	stored, _ := s.store.Message(m.ID)
	if b, _ := json.Marshal(stored); bytes.Contains(b, []byte(`"data"`)) {
		t.Fatalf("stored message carries data: %s", b)
	}
	if !s.blobs.has(sum) {
		t.Fatal("file bytes not kept")
	}
	mu.Lock()
	if len(got) != 1 || len(got[0].Files) != 1 || !bytes.Equal(got[0].Files[0].Data, patch) || got[0].Files[0].Name != "0001-x.patch" {
		t.Fatalf("delivered %+v", got)
	}
	mu.Unlock()

	// Any harness takes files: they are paths in the text.
	if msgs, err := r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: agy.Path, Text: "x", FileAttachments: files}); err != nil || msgs[0].State != model.MsgDelivered {
		t.Fatalf("to agy: %+v %v", msgs, err)
	}
	refused := func(args fleet.SendArgs, want string) {
		t.Helper()
		if args.Text == "" {
			args.Text = "x"
		}
		if _, err := r.send(ctx, from(a), model.MsgNotify, args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", args.To, err, want)
		}
	}
	refused(fleet.SendArgs{To: old.Path, FileAttachments: files}, "older swk node")
	refused(fleet.SendArgs{To: "user:jm", FileAttachments: files}, "agent sessions only")
	refused(fleet.SendArgs{To: "b", Files: []string{"fix.patch"}}, "older swk node") // paths reaching the coordinator
	refused(fleet.SendArgs{To: "b", FileAttachments: []model.Attachment{{Name: "x.txt", Data: []byte{0, 1}}}}, "not text")
	refused(fleet.SendArgs{To: "b", FileAttachments: make([]model.Attachment, 5)}, "at most 4")

	// In a send to several, the node that cannot take files is refused
	// alone.
	res, err := r.sendEach(ctx, from(a), model.MsgNotify, fleet.SendArgs{ToList: []string{"b", old.Path, "user:jm"}, Text: "x", FileAttachments: files})
	if err != nil || len(res.Messages) != 3 || res.Messages[0].State != model.MsgDelivered ||
		res.Messages[1].State != fleet.Refused || res.Messages[2].State != fleet.Refused {
		t.Fatalf("several: %+v %v", res, err)
	}

	// A message whose node no longer takes files (downgraded while it
	// waited) fails rather than arriving without them.
	pending := model.Message{ID: "01PENDINGFILES", Kind: model.MsgNotify, From: a.Path, To: "b", ToSession: old.ID, ToPath: old.Path,
		Text: "x", Files: m.Files, State: model.MsgPending, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt}
	fakeNode(t, s, "oldbox", func(wire.Cmd) wire.Result { return wire.Result{OK: true} })
	if err := s.store.PutMessage(pending); err != nil {
		t.Fatal(err)
	}
	if out := r.deliver(ctx, pending); out.State != model.MsgFailed || !strings.Contains(out.Error, "cannot take files") {
		t.Fatalf("downgraded node: %+v", out)
	}

	// Agents' files count against a per-minute byte budget of their own.
	big := []model.Attachment{{Name: "a.log", Data: bytes.Repeat([]byte("a"), attach.MaxFileBytes)},
		{Name: "b.log", Data: bytes.Repeat([]byte("b"), attach.MaxFileBytes)}}
	var limited error
	for range agentFileBytesPerMinute/(2*attach.MaxFileBytes) + 1 {
		if _, limited = r.send(ctx, from(a), model.MsgNotify, fleet.SendArgs{To: agy.Path, Text: "x", FileAttachments: big}); limited != nil {
			break
		}
	}
	if limited == nil || !strings.Contains(limited.Error(), "MiB of files a minute") {
		t.Fatalf("file budget: %v", limited)
	}

	// People's files count against a budget per credential, whatever
	// name they send as.
	limited = nil
	for i := range personFileBytesPerMinute/(2*attach.MaxFileBytes) + 1 {
		p := sender{user: "user:p" + string(rune('a'+i)), via: "token:laptop"}
		if _, limited = r.send(ctx, p, model.MsgNotify, fleet.SendArgs{To: agy.Path, Text: "x", FileAttachments: big}); limited != nil {
			break
		}
	}
	if limited == nil || !strings.Contains(limited.Error(), "each credential may send") {
		t.Fatalf("person's file budget: %v", limited)
	}
	if _, err := r.send(ctx, sender{user: "user:jm", via: "admin", owner: true}, model.MsgNotify, fleet.SendArgs{To: agy.Path, Text: "x", FileAttachments: big}); err != nil {
		t.Fatalf("another credential: %v", err)
	}
}
