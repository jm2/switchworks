// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

// A deleted session whose event log cannot be removed is still deleted,
// and its log is removed later: by RemoveDeletedLogs, or when the store
// opens again, never left behind.
func TestDeletedLogRetried(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	newSession := func(name string) string {
		id := ulid.New()
		if err := s.CreateSession(model.Session{ID: id, Path: "devbox/" + name, Host: "devbox", Name: name,
			Status: model.StatusStopped, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvents([]model.Event{{ID: ulid.New(), SessionID: id, Seq: 1, TS: time.Now(), Kind: model.KindWarning,
			Payload: model.Marshal(model.ErrorEvent{Message: "x"})}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	events := filepath.Join(dir, "events")
	logOf := func(id string) string { return filepath.Join(events, id+".jsonl") }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	a, b := newSession("a"), newSession("b")
	_ = os.Chmod(events, 0o500)
	errA := s.DeleteSession(a)
	errB := s.DeleteSession(b)
	if !errors.Is(errA, ErrLogPending) || !errors.Is(errB, ErrLogPending) {
		t.Fatalf("DeleteSession with an unwritable events dir: %v, %v", errA, errB)
	}
	if _, ok := s.Session(a); ok {
		t.Fatal("session kept")
	}
	if n := s.RemoveDeletedLogs(); n != 2 {
		t.Fatalf("RemoveDeletedLogs with the dir still unwritable = %d, want 2", n)
	}
	_ = os.Chmod(events, 0o700)

	// Retried while running.
	if n := s.RemoveDeletedLogs(); n != 0 || exists(logOf(a)) || exists(logOf(b)) {
		t.Fatalf("RemoveDeletedLogs = %d; logs still there: %v %v", n, exists(logOf(a)), exists(logOf(b)))
	}

	// Retried at the next open, from state.json.
	c := newSession("c")
	_ = os.Chmod(events, 0o500)
	if err := s.DeleteSession(c); !errors.Is(err, ErrLogPending) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(events, 0o700)
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if exists(logOf(c)) {
		t.Fatal("the pending log was not removed at open")
	}
	if n := s.RemoveDeletedLogs(); n != 0 {
		t.Fatalf("%d logs still pending after open", n)
	}
}

// A session created again under a deleted session's id does not inherit
// its log.
func TestCreateAfterPendingDelete(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := ulid.New()
	sess := model.Session{ID: id, Path: "devbox/x", Host: "devbox", Name: "x", Status: model.StatusStopped, CreatedAt: time.Now()}
	if err := s.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvents([]model.Event{{ID: ulid.New(), SessionID: id, Seq: 1, TS: time.Now(), Kind: model.KindWarning,
		Payload: model.Marshal(model.ErrorEvent{Message: "old"})}}); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(dir, "events")
	_ = os.Chmod(events, 0o500)
	if err := s.DeleteSession(id); !errors.Is(err, ErrLogPending) {
		t.Fatal(err)
	}
	// Still unwritable: refused rather than reusing the old log.
	if err := s.CreateSession(sess); err == nil {
		t.Fatal("created over a log that could not be removed")
	}
	_ = os.Chmod(events, 0o700)
	if err := s.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Events(id, 0, 10); len(evs) != 0 || s.LastSeq(id) != 0 {
		t.Fatalf("the new session has the old one's events: %v (last seq %d)", evs, s.LastSeq(id))
	}
}

// EndedAt is set by the store's clock when a session ends, whatever ends
// it, and cleared if it comes back.
func TestEndedAt(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := ulid.New()
	if err := s.CreateSession(model.Session{ID: id, Path: "devbox/x", Host: "devbox", Name: "x", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	sess, _ := s.UpdateSession(id, func(x *model.Session) { x.Status = model.StatusGone })
	if sess.EndedAt.Before(before) {
		t.Fatalf("EndedAt %v after going gone at %v", sess.EndedAt, before)
	}
	first := sess.EndedAt
	if sess, _ = s.UpdateSession(id, func(x *model.Session) { x.Status = model.StatusStopped }); !sess.EndedAt.Equal(first) {
		t.Fatalf("EndedAt moved from %v to %v between two ended states", first, sess.EndedAt)
	}
	if sess, _ = s.UpdateSession(id, func(x *model.Session) { x.Status = model.StatusIdle }); !sess.EndedAt.IsZero() {
		t.Fatalf("EndedAt %v kept on a live session", sess.EndedAt)
	}
}

// Tombstones survive a restart and expire after a year.
func TestRetiredPersistsAndExpires(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := ulid.New()
	if err := s.CreateSession(model.Session{ID: id, Path: "devbox/x", Host: "devbox", Name: "x", Status: model.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RetireSessions([]string{id}); err != nil || len(got) != 1 {
		t.Fatalf("RetireSessions = %v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = OpenFileStore(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Retired(id) {
		t.Fatal("tombstone lost across a restart")
	}
	s.mu.Lock()
	s.retired[id] = time.Now().Add(-retiredTTL - time.Hour)
	s.mu.Unlock()
	s.RemoveDeletedLogs()
	if s.Retired(id) {
		t.Fatal("a year-old tombstone kept")
	}
}
