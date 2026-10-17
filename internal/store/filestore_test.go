// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
)

const (
	s1 = "01JAAAAAAAAAAAAAAAAAAAAAA1"
	s2 = "01JAAAAAAAAAAAAAAAAAAAAAA2"
	s3 = "01JAAAAAAAAAAAAAAAAAAAAAA3"
)

func ev(session string, seq int64, kind string) model.Event {
	return model.Event{ID: kind + "-" + string(rune('a'+seq)), SessionID: session, Seq: seq, TS: time.Now(), Kind: kind}
}

func TestEventsIdempotentAndPersistent(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a", Status: model.StatusStarting}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(model.Session{ID: s2, Path: "devbox/a"}); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("want ErrPathInUse, got %v", err)
	}
	stored, err := s.AppendEvents([]model.Event{ev(s1, 1, "a"), ev(s1, 2, "b"), ev(s1, 3, "c")})
	if err != nil || len(stored) != 3 {
		t.Fatalf("append: %v %d", err, len(stored))
	}
	// Replay with overlap: only seq 4 is new.
	stored, err = s.AppendEvents([]model.Event{ev(s1, 2, "b"), ev(s1, 3, "c"), ev(s1, 4, "d")})
	if err != nil || len(stored) != 1 || stored[0].Seq != 4 {
		t.Fatalf("idempotent append: %v %+v", err, stored)
	}
	got, _ := s.Events(s1, 1, 2)
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("events after 1 limit 2: %+v", got)
	}
	if sess, _ := s.Session(s1); sess.LastSeq != 4 {
		t.Fatalf("last seq %d", sess.LastSeq)
	}
	if _, err := s.UpdateSession(s1, func(x *model.Session) { x.Status = model.StatusStopped }); err != nil {
		t.Fatal(err)
	}
	// A stopped session's path can be reused; SessionByPath returns the newest.
	if err := s.CreateSession(model.Session{ID: s3, Path: "devbox/a"}); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.SessionByPath("devbox/a"); sess.ID != s3 {
		t.Fatalf("by path: %s", sess.ID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a torn final write, then reopen.
	f, _ := os.OpenFile(filepath.Join(dir, "events", s1+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":"torn","session_id":s1,"seq":5`)
	f.Close()
	s2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.LastSeq(s1) != 4 {
		t.Fatalf("after reopen last seq %d", s2.LastSeq(s1))
	}
	all, _ := s2.Events(s1, 0, 0)
	if len(all) != 4 {
		t.Fatalf("after reopen %d events", len(all))
	}
	if _, err := s2.AppendEvents([]model.Event{ev(s1, 5, "e")}); err != nil {
		t.Fatal(err)
	}
	if all, _ := s2.Events(s1, 4, 0); len(all) != 1 || all[0].Kind != "e" {
		t.Fatalf("append after torn line: %+v", all)
	}
	if sess, ok := s2.Session(s1); !ok || sess.Status != model.StatusStopped || sess.LastSeq != 5 {
		t.Fatalf("session after reopen: %+v", sess)
	}
}

func TestTokens(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.AddToken("h1", "devbox", time.Now().Add(time.Hour))
	_ = s.AddToken("h2", "mini", time.Now().Add(-time.Second))
	if host, err := s.ConsumeToken("h1"); err != nil || host != "devbox" {
		t.Fatalf("consume: %v %s", err, host)
	}
	if _, err := s.ConsumeToken("h1"); !errors.Is(err, ErrBadToken) {
		t.Fatal("token reused")
	}
	if _, err := s.ConsumeToken("h2"); !errors.Is(err, ErrBadToken) {
		t.Fatal("expired token accepted")
	}
}

func TestRejectsUnsafeSessionIDs(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"../../escape", "x/../" + s1, strings.ToLower(s1), "./" + s1, ""} {
		if _, err := s.AppendEvents([]model.Event{ev(id, 1, "a")}); err == nil {
			t.Errorf("AppendEvents accepted session id %q", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.jsonl")); err == nil {
		t.Fatal("event file written outside the store")
	}
}

// Finished messages beyond the byte budget are dropped oldest first, and
// the file is compacted while running.
func TestPruneMessagesBudget(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	text := strings.Repeat("x", 1<<20)
	now := time.Now()
	n := maxMessageBytes/(1<<20) + 20
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("01J%023d", i)
		m := model.Message{ID: id, Kind: model.MsgNotify, State: model.MsgPending, Text: text, CreatedAt: now}
		_ = s.PutMessage(m)
		m.State = model.MsgDelivered
		_ = s.PutMessage(m)
	}
	s.PruneMessages(now)
	total := 0
	for _, m := range s.Messages(MessageFilter{}) {
		total += len(m.Text)
	}
	if total > maxMessageBytes {
		t.Fatalf("%d bytes of messages kept", total)
	}
	if _, ok := s.Message(fmt.Sprintf("01J%023d", 0)); ok {
		t.Fatal("the oldest message survived")
	}
	if _, ok := s.Message(fmt.Sprintf("01J%023d", n-1)); !ok {
		t.Fatal("the newest message was dropped")
	}
}

// A running session that passes through "error" (its harness restarting)
// keeps its path; one whose spawn failed does not.
func TestErrorPathReuse(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref := &model.NativeRef{Harness: "codex", ID: "t1"}
	_ = s.CreateSession(model.Session{ID: s1, Path: "devbox/a", Status: model.StatusError, NativeRef: ref})
	if err := s.CreateSession(model.Session{ID: s2, Path: "devbox/a"}); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("path of a running session in error reused: %v", err)
	}
	_ = s.CreateSession(model.Session{ID: s3, Path: "devbox/b", Status: model.StatusError})
	if err := s.CreateSession(model.Session{ID: "01JAAAAAAAAAAAAAAAAAAAAAA4", Path: "devbox/b", Status: model.StatusIdle}); err != nil {
		t.Fatalf("path of a failed spawn not reusable: %v", err)
	}
	for i := 0; i < 50; i++ {
		if got, _ := s.SessionByPath("devbox/b"); got.ID != "01JAAAAAAAAAAAAAAAAAAAAAA4" {
			t.Fatalf("SessionByPath = %s", got.ID)
		}
	}
}

// A record torn by a crash must not swallow the next one.
func TestTornMessageTail(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a := model.Message{ID: "01J00000000000000000000001", Kind: model.MsgNotify, State: model.MsgPending, Text: "a", CreatedAt: now}
	if err := s.PutMessage(a); err != nil {
		t.Fatal(err)
	}
	s.Close()
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"01J0000000000000000000000X","kind":"no`)
	f.Close()

	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ID, b.Text = "01J00000000000000000000002", "b"
	if err := s.PutMessage(b); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{a.ID, b.ID} {
		if _, ok := s.Message(id); !ok {
			t.Fatalf("message %s lost after a torn tail", id)
		}
	}
}

func TestDeleteSession(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a", Status: model.StatusStarting}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvents([]model.Event{ev(s1, 1, "a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(s1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(s1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, ok := s.Session(s1); ok || len(s.Sessions()) != 0 {
		t.Fatal("session still listed")
	}
	if _, err := os.Stat(filepath.Join(dir, "events", s1+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event log kept: %v", err)
	}
	if evs, _ := s.Events(s1, 0, 0); len(evs) != 0 || s.LastSeq(s1) != 0 {
		t.Fatalf("events kept: %+v", evs)
	}
	// The path is free, and the deletion survives a reopen.
	if err := s.CreateSession(model.Session{ID: s2, Path: "devbox/a", Status: model.StatusStarting}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if all := s.Sessions(); len(all) != 1 || all[0].ID != s2 {
		t.Fatalf("after reopen: %+v", all)
	}
}
