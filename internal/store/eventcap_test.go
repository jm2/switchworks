// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/model"
)

// bigEv is an event of about n bytes.
func bigEv(session string, seq int64, n int) model.Event {
	e := ev(session, seq, "item.delta")
	e.Payload, _ = json.Marshal(map[string]string{"text": strings.Repeat("x", n)})
	return e
}

// projected marks every stored event of id projected, as the coordinator
// does after each append.
func projected(t *testing.T, s *FileStore, id string) {
	t.Helper()
	last := s.LastSeq(id)
	if _, err := s.UpdateSession(id, func(x *model.Session) { x.ProjectedSeq = last }); err != nil {
		t.Fatal(err)
	}
}

// A log past the cap drops its oldest events behind a warning that says
// so; seqs, LastSeq, paging and appends go on as before, also after a
// restart.
func TestEventLogCap(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const capBytes = 64 << 10
	s.SetEventLogCap(capBytes)
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events", s1+".jsonl")
	for seq := int64(1); seq <= 200; seq++ {
		if stored, err := s.AppendEvents([]model.Event{bigEv(s1, seq, 1000)}); err != nil || len(stored) != 1 {
			t.Fatalf("append %d: %v %d", seq, err, len(stored))
		}
		projected(t, s, s1)
		if st, err := os.Stat(path); err != nil || st.Size() > capBytes+2000 {
			t.Fatalf("after %d: log is %d bytes (%v), cap %d", seq, st.Size(), err, capBytes)
		}
	}
	if s.LastSeq(s1) != 200 {
		t.Fatalf("last seq %d", s.LastSeq(s1))
	}
	check := func(s *FileStore) {
		t.Helper()
		all, err := s.Events(s1, 0, 0)
		if err != nil || len(all) < 2 {
			t.Fatalf("events: %v %d", err, len(all))
		}
		m := all[0]
		var p TruncatedEvent
		if m.Kind != model.KindWarning || m.Source != TruncatedSource || json.Unmarshal(m.Payload, &p) != nil || p.Through != m.Seq || p.Message == "" {
			t.Fatalf("first event is not the truncation warning: %+v", m)
		}
		// The rest run without a gap from just past the marker to the end.
		for i, e := range all[1:] {
			if e.Seq != m.Seq+1+int64(i) {
				t.Fatalf("event %d has seq %d after marker %d", i, e.Seq, m.Seq)
			}
		}
		if all[len(all)-1].Seq != 200 {
			t.Fatalf("last event %d", all[len(all)-1].Seq)
		}
		// A reader past the marker misses nothing and sees no marker.
		if got, _ := s.Events(s1, 190, 5); len(got) != 5 || got[0].Seq != 191 || got[4].Seq != 195 {
			t.Fatalf("page after 190: %+v", got)
		}
		// rebuildActivity's read of the last 400.
		if got, _ := s.Events(s1, 200-400, 400); len(got) != len(all) {
			t.Fatalf("tail read %d, want %d", len(got), len(all))
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.LastSeq(s1) != 200 {
		t.Fatalf("last seq after reopen %d", s.LastSeq(s1))
	}
	check(s)
	// Duplicates are still ignored, and the next seq is stored.
	if stored, err := s.AppendEvents([]model.Event{bigEv(s1, 150, 10), bigEv(s1, 200, 10), bigEv(s1, 201, 10)}); err != nil || len(stored) != 1 || stored[0].Seq != 201 {
		t.Fatalf("append after reopen: %v %+v", err, stored)
	}
	if got, _ := s.Events(s1, 200, 0); len(got) != 1 || got[0].Seq != 201 {
		t.Fatalf("after 200: %+v", got)
	}
}

// Events the coordinator has not projected yet are never dropped: after a
// crash, replayProjections reads them.
func TestEventLogCapKeepsUnprojected(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetEventLogCap(16 << 10)
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a"}); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 100; seq++ {
		if _, err := s.AppendEvents([]model.Event{bigEv(s1, seq, 1000)}); err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := s.Events(s1, 0, 0); len(all) != 100 || all[0].Seq != 1 {
		t.Fatalf("unprojected events dropped: %d, first %d", len(all), all[0].Seq)
	}
	// Projected up to 50: only those can go.
	if _, err := s.UpdateSession(s1, func(x *model.Session) { x.ProjectedSeq = 50 }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvents([]model.Event{bigEv(s1, 101, 1000)}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Events(s1, 0, 0)
	if len(all) != 52 || all[0].Seq != 50 || all[1].Seq != 51 {
		t.Fatalf("after projecting to 50: %d events, first %d, %d", len(all), all[0].Seq, all[1].Seq)
	}
}

// A cap of 0 turns compaction off; a log being read is not compacted
// under its reader.
func TestEventLogCapOffAndBusy(t *testing.T) {
	s, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetEventLogCap(0)
	for seq := int64(1); seq <= 40; seq++ {
		if _, err := s.AppendEvents([]model.Event{bigEv(s1, seq, 1000)}); err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := s.Events(s1, 0, 0); len(all) != 40 {
		t.Fatalf("no cap: %d events", len(all))
	}
	s.SetEventLogCap(8 << 10)
	s.mu.Lock()
	s.logs[s1].pins++ // a reader
	s.mu.Unlock()
	if _, err := s.AppendEvents([]model.Event{bigEv(s1, 41, 1000)}); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Events(s1, 0, 0); len(all) != 41 {
		t.Fatalf("compacted under a reader: %d events", len(all))
	}
	s.mu.Lock()
	s.logs[s1].pins--
	s.mu.Unlock()
	if _, err := s.AppendEvents([]model.Event{bigEv(s1, 42, 1000)}); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Events(s1, 0, 0); len(all) >= 42 || all[len(all)-1].Seq != 42 {
		t.Fatalf("not compacted once free: %d events", len(all))
	}
}

// A compaction's temporary file left by a crash is removed on open.
func TestEventLogCapStaleTemp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "events", s1+".jsonl.tmp")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("stale temp file kept: %v", err)
	}
}

func TestDiskSpace(t *testing.T) {
	free, total, err := DiskSpace(t.TempDir())
	if err != nil {
		t.Skipf("not supported here: %v", err)
	}
	if total == 0 || free > total {
		t.Fatalf("free %d of %d", free, total)
	}
}

// The last usage event is kept through compactions, ahead of the warning:
// after a restart, the coordinator counts the next one's growth from it.
func TestEventLogCapKeepsLastUsage(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.SetEventLogCap(32 << 10)
	if err := s.CreateSession(model.Session{ID: s1, Path: "devbox/a"}); err != nil {
		t.Fatal(err)
	}
	usage := ev(s1, 3, model.KindUsage)
	usage.Payload = json.RawMessage(`{"total":{"total_tokens":42}}`)
	for seq := int64(1); seq <= 150; seq++ {
		e := bigEv(s1, seq, 1000)
		if seq == 3 {
			e = usage
		}
		if _, err := s.AppendEvents([]model.Event{e}); err != nil {
			t.Fatal(err)
		}
		projected(t, s, s1)
	}
	check := func(s *FileStore) {
		t.Helper()
		all, _ := s.Events(s1, 0, 0)
		if len(all) < 3 || all[0].Seq != 3 || all[0].Kind != model.KindUsage || string(all[0].Payload) != string(usage.Payload) {
			t.Fatalf("usage event not kept first: %+v", all[0])
		}
		if all[1].Kind != model.KindWarning || all[1].Seq <= 3 || all[2].Seq != all[1].Seq+1 || all[len(all)-1].Seq != s.LastSeq(s1) {
			t.Fatalf("after the usage event: %+v %+v", all[1], all[2])
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopened, it is found again, and kept by the next compactions.
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetEventLogCap(32 << 10)
	for seq := int64(151); seq <= 250; seq++ {
		if _, err := s.AppendEvents([]model.Event{bigEv(s1, seq, 1000)}); err != nil {
			t.Fatal(err)
		}
		projected(t, s, s1)
	}
	check(s)
	if all, _ := s.Events(s1, 0, 0); all[len(all)-1].Seq != 250 || all[1].Seq < 151 {
		t.Fatalf("not compacted again: warning at %d, last %d", all[1].Seq, all[len(all)-1].Seq)
	}
}
