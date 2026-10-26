// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

// failWrites makes the next n writes to store files write half their bytes
// and fail, as a full disk (or a file size limit) does.
func failWrites(t *testing.T, n int) {
	t.Helper()
	orig := writeFile
	t.Cleanup(func() { writeFile = orig })
	writeFile = func(f *os.File, b []byte) (int, error) {
		if n == 0 {
			return orig(f, b)
		}
		n--
		k, _ := f.Write(b[:len(b)/2])
		return k, syscall.ENOSPC
	}
}

func seqs(evs []model.Event) []int64 {
	var out []int64
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

// A write that fails partway leaves no partial line behind: the resent
// event is stored whole, readable now and after a reopen.
func TestPartialEventWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvents([]model.Event{ev(s1, 1, "a"), ev(s1, 2, "b")}); err != nil {
		t.Fatal(err)
	}
	failWrites(t, 1)
	// The batch's events in that log are all dropped, so none is acked.
	stored, err := s.AppendEvents([]model.Event{ev(s1, 3, "c"), ev(s1, 4, "d")})
	if err == nil {
		t.Fatal("no error from a failed write")
	}
	if len(stored) != 0 || s.LastSeq(s1) != 2 {
		t.Fatalf("after a failed write: stored %v, last seq %d", seqs(stored), s.LastSeq(s1))
	}
	// The node resends.
	stored, err = s.AppendEvents([]model.Event{ev(s1, 3, "c"), ev(s1, 4, "d")})
	if err != nil || len(stored) != 2 {
		t.Fatalf("resend: %v %v", err, seqs(stored))
	}
	got, err := s.Events(s1, 0, 0)
	if err != nil || fmt.Sprint(seqs(got)) != "[1 2 3 4]" {
		t.Fatalf("events: %v %v", err, seqs(got))
	}
	s.Close()
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err = s.Events(s1, 0, 0)
	if err != nil || fmt.Sprint(seqs(got)) != "[1 2 3 4]" || s.LastSeq(s1) != 4 {
		t.Fatalf("after reopen: %v %v last %d", err, seqs(got), s.LastSeq(s1))
	}
}

// A write that fails partway after others in the same batch succeeded
// keeps the events of the other sessions.
func TestPartialEventWriteOtherSessions(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvents([]model.Event{ev(s1, 1, "a"), ev(s2, 1, "a")}); err != nil {
		t.Fatal(err)
	}
	orig := writeFile
	t.Cleanup(func() { writeFile = orig })
	calls := 0
	writeFile = func(f *os.File, b []byte) (int, error) {
		calls++
		if calls == 2 {
			k, _ := f.Write(b[:len(b)/2])
			return k, syscall.EFBIG
		}
		return orig(f, b)
	}
	stored, err := s.AppendEvents([]model.Event{ev(s2, 2, "b"), ev(s1, 2, "b")})
	if err == nil || len(stored) != 1 || stored[0].SessionID != s2 {
		t.Fatalf("stored %+v, err %v", stored, err)
	}
	if s.LastSeq(s1) != 1 || s.LastSeq(s2) != 2 {
		t.Fatalf("last seqs %d %d", s.LastSeq(s1), s.LastSeq(s2))
	}
	if sess, _ := s.Session(s1); sess.LastSeq != 1 {
		t.Fatalf("session last seq %d", sess.LastSeq)
	}
	writeFile = orig
	if _, err := s.AppendEvents([]model.Event{ev(s1, 2, "b")}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Events(s1, 0, 0); err != nil || fmt.Sprint(seqs(got)) != "[1 2]" {
		t.Fatalf("events: %v %v", err, seqs(got))
	}
}

// A log damaged by an older version (a partial line with the next event
// glued onto it) still reads: the damaged line is skipped, not the range.
func TestEventsSkipDamagedLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(seq int64) string {
		b, _ := json.Marshal(ev(s1, seq, "k"))
		return string(b) + "\n"
	}
	l3 := line(3)
	body := line(1) + line(2) + l3[:20] + line(4) + line(5)
	if err := os.WriteFile(filepath.Join(dir, "events", s1+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Events(s1, 0, 0)
	if err != nil || fmt.Sprint(seqs(got)) != "[1 2 5]" {
		t.Fatalf("events: %v %v", err, seqs(got))
	}
	if got, err := s.Events(s1, 1, 1); err != nil || fmt.Sprint(seqs(got)) != "[2]" {
		t.Fatalf("events after 1: %v %v", err, seqs(got))
	}
}

// A message record whose write fails partway is cut off, so the next one
// is not glued onto it.
func TestPartialMessageWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	msg := func(id string) model.Message {
		return model.Message{ID: id, Kind: model.MsgNotify, State: model.MsgPending, Text: id, CreatedAt: now}
	}
	a, b, c := "01J00000000000000000000001", "01J00000000000000000000002", "01J00000000000000000000003"
	if err := s.PutMessage(msg(a)); err != nil {
		t.Fatal(err)
	}
	failWrites(t, 1)
	if err := s.PutMessage(msg(b)); err == nil {
		t.Fatal("no error from a failed write")
	}
	if _, ok := s.Message(b); ok {
		t.Fatal("failed message kept")
	}
	if err := s.PutMessage(msg(c)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Fatalf("%d lines in messages.jsonl:\n%s", n, raw)
	}
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{a, c} {
		if _, ok := s.Message(id); !ok {
			t.Fatalf("message %s lost", id)
		}
	}
}

// A session whose creation could not be saved does not stay behind to
// hold its path.
func TestCreateSessionSaveFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err = s.CreateSession(model.Session{ID: s1, Path: "devbox/a", Status: model.StatusStarting})
	_ = os.Chmod(dir, 0o700)
	if err == nil {
		t.Fatal("creation saved into a read-only directory")
	}
	if _, ok := s.Session(s1); ok {
		t.Fatal("unsaved session kept")
	}
	if err := s.CreateSession(model.Session{ID: s2, Path: "devbox/a", Status: model.StatusStarting}); err != nil {
		t.Fatalf("path held by the unsaved session: %v", err)
	}
}

// Logs are opened when used, never more than maxOpenLogs at once, and
// their last seqs are known without opening them.
func TestLazyEventLogs(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := maxOpenLogs + 10
	id := func(i int) string { return fmt.Sprintf("01JAAAAAAAAAAAAAAAAAAA%04d", i) }
	for i := 0; i < n; i++ {
		if _, err := s.AppendEvents([]model.Event{ev(id(i), 1, "a"), ev(id(i), 2, "b")}); err != nil {
			t.Fatal(err)
		}
	}
	if s.openLogs > maxOpenLogs {
		t.Fatalf("%d logs open", s.openLogs)
	}
	s.Close()
	// A last line longer than the tail read is found by indexing.
	big := ev(id(0), 3, "big")
	big.Payload = json.RawMessage(`"` + strings.Repeat("x", tailBytes) + `"`)
	b, _ := json.Marshal(big)
	f, _ := os.OpenFile(filepath.Join(dir, "events", id(0)+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write(append(b, '\n'))
	f.Close()

	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.openLogs > 1 {
		t.Fatalf("%d logs opened at startup", s.openLogs)
	}
	for i := 0; i < n; i++ {
		want := int64(2)
		if i == 0 {
			want = 3
		}
		if got := s.LastSeq(id(i)); got != want {
			t.Fatalf("log %d: last seq %d", i, got)
		}
	}
	for i := 0; i < n; i++ {
		if got, err := s.Events(id(i), 0, 0); err != nil || len(got) < 2 {
			t.Fatalf("log %d: %v %v", i, err, seqs(got))
		}
		if _, err := s.AppendEvents([]model.Event{ev(id(i), 4, "c")}); err != nil {
			t.Fatal(err)
		}
		if s.openLogs > maxOpenLogs {
			t.Fatalf("%d logs open", s.openLogs)
		}
	}
	if got, _ := s.Events(id(1), 0, 0); fmt.Sprint(seqs(got)) != "[1 2 4]" {
		t.Fatalf("reopened log: %v", seqs(got))
	}
}

// Asks delivered but never answered are dropped past the retention like
// finished messages.
func TestPruneUnansweredAsks(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-messageRetention - time.Hour)
	ask := model.Message{ID: "01J00000000000000000000001", Kind: model.MsgAsk, State: model.MsgDelivered, Text: "q", CreatedAt: old}
	pending := model.Message{ID: "01J00000000000000000000002", Kind: model.MsgAsk, State: model.MsgPending, Text: "q", CreatedAt: old}
	for _, m := range []model.Message{ask, pending} {
		if err := s.PutMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	s.PruneMessages(time.Now())
	if _, ok := s.Message(ask.ID); ok {
		t.Fatal("old unanswered ask kept")
	}
	if _, ok := s.Message(pending.ID); !ok {
		t.Fatal("pending ask dropped")
	}
}

// A snapshot from before projections were tracked counts what it saved as
// projected; a current one keeps its own mark.
func TestProjectedSeqMigration(t *testing.T) {
	for _, tc := range []struct {
		version int
		want    int64
	}{{0, 5}, {snapshotVersion, 3}} {
		dir := t.TempDir()
		snap := snapshot{Version: tc.version, Sessions: []*model.Session{{ID: s1, Path: "devbox/a", LastSeq: 5, ProjectedSeq: 3}}}
		b, _ := json.Marshal(snap)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if sess, _ := s.Session(s1); sess.ProjectedSeq != tc.want {
			t.Errorf("version %d: projected seq %d, want %d", tc.version, sess.ProjectedSeq, tc.want)
		}
		s.Close()
	}
}

// A store from before read marks has its messages to people marked read,
// but for the asks still open, once; a new store's are left alone.
func TestReadMarksOnUpgrade(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	msgs := []model.Message{
		{ID: ulid.New(), Kind: model.MsgNotify, ToUser: "user:jm", State: model.MsgDelivered, CreatedAt: now, DeliveredAt: now.Add(time.Second)},
		{ID: ulid.New(), Kind: model.MsgAsk, ToUser: "user:jm", State: model.MsgDelivered, CreatedAt: now, DeliveredAt: now},
		{ID: ulid.New(), Kind: model.MsgAsk, ToUser: "user:jm", State: model.MsgAnswered, CreatedAt: now, DeliveredAt: now},
		{ID: ulid.New(), Kind: model.MsgNotify, ToSession: s1, State: model.MsgDelivered, CreatedAt: now, DeliveredAt: now},
	}
	for _, version := range []int{0, 1, -1} {
		dir := t.TempDir()
		if version >= 0 {
			b, _ := json.Marshal(snapshot{Version: version})
			if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var lines []byte
		for _, m := range msgs {
			b, _ := json.Marshal(m)
			lines = append(append(lines, b...), '\n')
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), lines, 0o600); err != nil {
			t.Fatal(err)
		}
		for open := 0; open < 2; open++ {
			s, err := OpenFileStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			note, _ := s.Message(msgs[0].ID)
			ask, _ := s.Message(msgs[1].ID)
			answered, _ := s.Message(msgs[2].ID)
			toSession, _ := s.Message(msgs[3].ID)
			if !note.ReadAt.Equal(msgs[0].DeliveredAt) || note.ReadBy != upgradeReader || !answered.ReadAt.Equal(now) {
				t.Errorf("version %d: old messages not read: %+v %+v", version, note, answered)
			}
			if !ask.ReadAt.IsZero() || !toSession.ReadAt.IsZero() {
				t.Errorf("version %d: open ask or a session's message marked read: %+v %+v", version, ask, toSession)
			}
			if open == 0 {
				// A new notify after the upgrade stays new on the next open.
				fresh := model.Message{ID: ulid.New(), Kind: model.MsgNotify, ToUser: "user:jm", State: model.MsgDelivered, CreatedAt: now}
				if err := s.PutMessage(fresh); err != nil {
					t.Fatal(err)
				}
				msgs = append(msgs, fresh)
			} else if m, _ := s.Message(msgs[len(msgs)-1].ID); !m.ReadAt.IsZero() {
				t.Errorf("version %d: marked read again on the next open: %+v", version, m)
			}
			s.Close()
		}
		msgs = msgs[:4]
	}

	// A new store, and one already at this version, mark nothing.
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutMessage(msgs[0]); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, _ := s.Message(msgs[0].ID); !m.ReadAt.IsZero() {
		t.Fatalf("a current store's message marked read: %+v", m)
	}
}
