// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

// FileStore keeps metadata in a JSON snapshot (state.json, rewritten
// atomically) and each session's events in an append-only JSONL file
// (events/<session-id>.jsonl) with an in-memory seq->offset index.
//
// Durability: AppendEvents fsyncs the touched event files before returning,
// because nodes drop their spool once the coordinator acknowledges. Metadata
// changes are flushed at most every 500ms (immediately for session creation
// and tokens); on startup, each session's last seq is recovered from its
// event file, and the coordinator projects again the events after the
// session's ProjectedSeq, so a crash loses no events and no projected state.
//
// Event files are opened when first used, at most maxOpenLogs at a time
// (least recently used first out); a closed one keeps only its last seq.
//
// An event file that grows past the log cap (SetEventLogCap) is compacted
// on append: its oldest events are dropped, keeping about half the cap,
// behind a warning event that says so (see compactLocked).
type FileStore struct {
	dir string

	mu       sync.Mutex
	hosts    map[string]model.Host
	sessions map[string]*model.Session
	tokens   map[string]tokenRec
	apiToks  map[string]APIToken // by id
	logs     map[string]*eventLog
	openLogs int    // logs with an open file
	clock    uint64 // orders log use, for eviction
	daily    map[string]int64
	// deletedLogs: event logs of deleted sessions still to be removed.
	deletedLogs map[string]bool
	retired     map[string]time.Time // tombstones, by session id
	dirty       bool
	gen         uint64 // snapshot generation, under mu
	logCap      int64  // bytes per event file (0: no cap)

	// saveMu orders snapshot writes: one taken later (a higher gen) is
	// never overwritten by an earlier one still being written.
	saveMu   sync.Mutex
	savedGen uint64

	msgs     map[string]*model.Message
	msgLog   *os.File
	msgSize  int64 // bytes of whole records in messages.jsonl
	msgTorn  bool  // a failed write left a partial record: rewrite first
	msgLines int

	stop chan struct{}
	done chan struct{}
}

type tokenRec struct {
	Hash    string    `json:"hash"`
	Host    string    `json:"host"`
	Expires time.Time `json:"expires"`
}

type snapshot struct {
	// Version 1: sessions carry ProjectedSeq.
	Version     int              `json:"version,omitempty"`
	Hosts       []model.Host     `json:"hosts"`
	Sessions    []*model.Session `json:"sessions"`
	Tokens      []tokenRec       `json:"tokens"`
	APITokens   []APIToken       `json:"api_tokens,omitempty"`
	DailyTokens map[string]int64 `json:"daily_tokens,omitempty"`
	// DeletedLogs are deleted sessions whose event logs could not be
	// removed yet; removal is tried again until it works.
	DeletedLogs []string `json:"deleted_logs,omitempty"`
	// Retired are the tombstones of sessions removed for good, with when.
	Retired map[string]time.Time `json:"retired,omitempty"`
}

// retiredTTL is how long a removed session's tombstone is kept: a node
// that still reports the session after that is long gone.
const retiredTTL = 365 * 24 * time.Hour

// retireBatch bounds the sessions one RetireSessions call removes, each
// call rewriting state.json once.
const retireBatch = 200

const snapshotVersion = 1

// maxEventsRead bounds the bytes one Events call reads.
const maxEventsRead = 16 << 20

// maxOpenLogs bounds the event files held open at once.
const maxOpenLogs = 64

// DefaultEventLogCap is the default size cap of one session's event file.
const DefaultEventLogCap = 256 << 20

// Finished messages (and asks delivered but never answered) older than
// this are dropped.
const messageRetention = 14 * 24 * time.Hour

// writeFile writes to a store file; tests replace it to fail writes.
var writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

var logf = log.Printf

type idx struct {
	seq int64
	off int64
}

type eventLog struct {
	id      string // the session's
	path    string
	f       *os.File // nil while closed
	index   []idx    // sorted by seq; nil while closed
	size    int64
	lastSeq int64 // known while closed too
	pins    int   // users outside the store lock (or mid-append): not evicted
	used    uint64
	// usage is the seq of the last usage event (0: none), while open: a
	// compaction keeps it, the baseline the coordinator counts the next
	// one's growth from after a restart.
	usage int64
}

func (l *eventLog) last() int64 { return l.lastSeq }

// OpenFileStore opens or creates a store in dir.
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		return nil, err
	}
	s := &FileStore{
		dir: dir, logCap: DefaultEventLogCap, hosts: map[string]model.Host{}, sessions: map[string]*model.Session{},
		tokens: map[string]tokenRec{}, apiToks: map[string]APIToken{}, logs: map[string]*eventLog{}, daily: map[string]int64{},
		deletedLogs: map[string]bool{}, retired: map[string]time.Time{},
		msgs: map[string]*model.Message{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		var snap snapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			return nil, fmt.Errorf("state.json: %w", err)
		}
		for _, h := range snap.Hosts {
			h.Connected = false
			s.hosts[h.ID] = h
		}
		for _, sess := range snap.Sessions {
			if snap.Version < 1 {
				// Saved before projections were tracked: take what was
				// saved as projected, as it was then.
				sess.ProjectedSeq = sess.LastSeq
			}
			if Ended(*sess) && sess.EndedAt.IsZero() {
				// Saved before EndedAt: counted from now.
				sess.EndedAt = time.Now()
			}
			s.sessions[sess.ID] = sess
		}
		for _, t := range snap.Tokens {
			s.tokens[t.Hash] = t
		}
		s.sweepTokensLocked(time.Now())
		for _, t := range snap.APITokens {
			s.apiToks[t.ID] = t
		}
		for d, n := range snap.DailyTokens {
			s.daily[d] = n
		}
		for _, id := range snap.DeletedLogs {
			if ulid.Valid(id) && s.sessions[id] == nil {
				s.deletedLogs[id] = true
			}
		}
		for id, at := range snap.Retired {
			if s.sessions[id] == nil {
				s.retired[id] = at
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := s.openMessages(); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	// Recover each log's last seq from its tail; logs are indexed when
	// first used.
	entries, err := os.ReadDir(filepath.Join(dir, "events"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl.tmp") {
			// A compaction cut short by a crash; the log itself is whole.
			_ = os.Remove(filepath.Join(dir, "events", e.Name()))
			continue
		}
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".jsonl")]
		if !ulid.Valid(id) {
			continue
		}
		l := &eventLog{id: id, path: filepath.Join(dir, "events", e.Name())}
		seq, ok, err := tailSeq(l.path)
		if err != nil {
			return nil, fmt.Errorf("events %s: %w", id, err)
		}
		if !ok {
			// No whole event in the tail (a huge last line): index it all.
			if err := s.openLocked(l); err != nil {
				return nil, fmt.Errorf("events %s: %w", id, err)
			}
			seq = l.last()
		}
		l.lastSeq = seq
		s.logs[id] = l
		if sess := s.sessions[id]; sess != nil {
			sess.LastSeq = seq
		}
	}
	if len(s.deletedLogs) > 0 {
		s.mu.Lock()
		if n := s.removeDeletedLogsLocked(); n > 0 {
			logf("store: %d event logs of deleted sessions could not be removed yet", n)
		}
		s.mu.Unlock()
	}
	go s.flusher()
	return s, nil
}

// tailBytes is how much of a log's end tailSeq reads.
const tailBytes = 256 << 10

// tailSeq finds the seq of the last whole, parsable event in the file at
// path from its end. ok is false when the tail holds none but the file is
// not empty.
func tailSeq(path string) (seq int64, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	n := min(st.Size(), tailBytes)
	if n == 0 {
		return 0, true, nil
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil && err != io.EOF {
		return 0, false, err
	}
	// Whole lines only: drop a torn last one, and (unless the tail is the
	// whole file) the first, which may be cut.
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return 0, n == st.Size(), nil
	}
	buf = buf[:end]
	for len(buf) > 0 {
		i := bytes.LastIndexByte(buf, '\n')
		if i < 0 && n < st.Size() {
			break
		}
		line := buf[i+1:]
		buf = buf[:max(i, 0)]
		var ev struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Seq > 0 {
			return ev.Seq, true, nil
		}
		if i < 0 {
			break
		}
	}
	return 0, n == st.Size(), nil
}

// openLocked opens and indexes l's file, cutting off a torn trailing line,
// and closes the least recently used other log past maxOpenLogs.
func (s *FileStore) openLocked(l *eventLog) error {
	s.clock++
	l.used = s.clock
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	var index []idx
	last := func() int64 {
		if len(index) == 0 {
			return 0
		}
		return index[len(index)-1].seq
	}
	br := bufio.NewReaderSize(f, 1<<20)
	var off, usage int64
	bad := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var ev struct {
				Seq  int64  `json:"seq"`
				Kind string `json:"kind"`
			}
			if json.Unmarshal(line, &ev) != nil {
				bad++
			} else if ev.Seq > last() {
				index = append(index, idx{ev.Seq, off})
				if ev.Kind == model.KindUsage {
					usage = ev.Seq
				}
			}
			off += int64(len(line))
		}
		if err != nil {
			// A torn trailing line (crash mid-write) is cut off.
			if err := f.Truncate(off); err != nil {
				f.Close()
				return err
			}
			break
		}
	}
	if bad > 0 {
		logf("store: %s: skipped %d unparsable lines", filepath.Base(l.path), bad)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	l.f, l.index, l.size, l.lastSeq, l.usage = f, index, off, last(), usage
	s.openLogs++
	for s.openLogs > maxOpenLogs {
		var lru *eventLog
		for _, o := range s.logs {
			if o.f != nil && o != l && o.pins == 0 && (lru == nil || o.used < lru.used) {
				lru = o
			}
		}
		if lru == nil {
			break // all in use; closed when they are not
		}
		s.closeLocked(lru)
	}
	return nil
}

// closeLocked closes l's file and drops its index; its last seq stays.
func (s *FileStore) closeLocked(l *eventLog) {
	if l.f == nil {
		return
	}
	_ = l.f.Close()
	l.f, l.index = nil, nil
	s.openLogs--
}

func (s *FileStore) flusher() {
	defer close(s.done)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stop:
			return
		}
	}
}

// flush saves a dirty snapshot. Only taking it holds the store lock: it is
// encoded and written outside, so the disk does not hold up the API and
// the nodes.
func (s *FileStore) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	snap, gen := s.snapshotLocked()
	s.dirty = false
	s.mu.Unlock()
	if err := s.writeSnapshot(snap, gen); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}

// saveLocked writes the snapshot now, for changes that must be durable
// before the caller goes on.
func (s *FileStore) saveLocked() error {
	snap, gen := s.snapshotLocked()
	if err := s.writeSnapshot(snap, gen); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// snapshotLocked copies the metadata into a snapshot that later changes do
// not touch.
func (s *FileStore) snapshotLocked() (snapshot, uint64) {
	snap := snapshot{Version: snapshotVersion}
	for _, h := range s.hosts {
		snap.Hosts = append(snap.Hosts, h)
	}
	sort.Slice(snap.Hosts, func(i, j int) bool { return snap.Hosts[i].ID < snap.Hosts[j].ID })
	for _, sess := range s.sessions {
		cp := *sess // updates replace its pointers and maps, never edit them
		snap.Sessions = append(snap.Sessions, &cp)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].ID < snap.Sessions[j].ID })
	now := time.Now()
	for _, t := range s.tokens {
		if t.Expires.After(now) {
			snap.Tokens = append(snap.Tokens, t)
		}
	}
	for _, t := range s.apiToks {
		if !t.Expired(now) {
			snap.APITokens = append(snap.APITokens, t)
		}
	}
	sort.Slice(snap.APITokens, func(i, j int) bool { return snap.APITokens[i].ID < snap.APITokens[j].ID })
	if len(s.daily) > 0 {
		snap.DailyTokens = map[string]int64{}
		cutoff := now.AddDate(0, 0, -90).Format("2006-01-02")
		for d, n := range s.daily {
			if d >= cutoff {
				snap.DailyTokens[d] = n
			}
		}
	}
	for id := range s.deletedLogs {
		snap.DeletedLogs = append(snap.DeletedLogs, id)
	}
	sort.Strings(snap.DeletedLogs)
	if len(s.retired) > 0 {
		snap.Retired = make(map[string]time.Time, len(s.retired))
		for id, at := range s.retired {
			snap.Retired[id] = at
		}
	}
	s.gen++
	return snap, s.gen
}

// writeSnapshot writes a snapshot unless a later one has been written.
func (s *FileStore) writeSnapshot(snap snapshot, gen uint64) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if gen <= s.savedGen {
		return nil
	}
	b, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(s.dir, "state.json"), b); err != nil {
		return err
	}
	s.savedGen = gen
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := writeFile(f, data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable. Windows cannot sync a directory
// (and does not need to).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Close flushes metadata and closes files.
func (s *FileStore) Close() error {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.saveLocked()
	for _, l := range s.logs {
		s.closeLocked(l)
	}
	if s.msgLog != nil {
		_ = s.msgLog.Close()
	}
	return err
}

// --- messages: messages.jsonl, one full record per line, last one wins ---

func (s *FileStore) msgPath() string { return filepath.Join(s.dir, "messages.jsonl") }

// prunable reports whether a message may be dropped once old: finished
// ones, and asks that were delivered but never answered (a reply after
// the retention would find nothing to answer, which is the same).
func prunable(m *model.Message) bool {
	return m.Terminal() || (m.Kind == model.MsgAsk && m.Accepted())
}

func (s *FileStore) openMessages() error {
	torn, bad := false, 0
	if f, err := os.Open(s.msgPath()); err == nil {
		br := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 && line[len(line)-1] != '\n' {
				// A crash mid-append. Appending after it would glue the
				// next record onto it, so the file is rewritten below.
				torn = true
			}
			if len(line) > 0 && line[len(line)-1] == '\n' {
				var m model.Message
				if json.Unmarshal(line, &m) == nil && m.ID != "" {
					cp := m
					s.msgs[m.ID] = &cp
					s.msgLines++
				} else {
					bad++
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return err
	}
	if bad > 0 {
		logf("store: messages.jsonl: skipped %d unparsable lines", bad)
	}
	// The same pruning as while running, so messages dropped for the byte
	// budget before a compaction could write that down stay dropped.
	s.pruneMessagesLocked(time.Now())
	// Compact: rewrite one line per live message.
	if s.msgLines > len(s.msgs) || torn || bad > 0 {
		return s.compactMessagesLocked()
	}
	return s.reopenMessagesLocked()
}

// reopenMessagesLocked opens messages.jsonl for appending.
func (s *FileStore) reopenMessagesLocked() error {
	f, err := os.OpenFile(s.msgPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.msgLog, s.msgSize = f, st.Size()
	return nil
}

// maxMessageBytes bounds what stored messages hold: past it, the oldest
// finished messages go first, however recent.
const maxMessageBytes = 256 << 20

// messageBytes is what a message counts against maxMessageBytes: its text
// and everything else of variable size its record keeps, the metadata of
// its images and files among it. Their bytes are in the attachment store,
// under that store's own limits, and are not counted twice here.
func messageBytes(m *model.Message) int {
	n := len(m.Text) + len(m.Error) + len(m.From) + len(m.To) + len(m.ToPath) + len(m.Via) + len(m.SeenTurn) + len(m.SeenItem)
	for _, list := range [][]model.Attachment{m.Attachments, m.Files} {
		for _, a := range list {
			n += len(a.Name) + len(a.MIME) + len(a.SHA256) + len(a.Data) + 32
		}
	}
	return n
}

// PruneMessages drops finished messages older than the retention, then the
// oldest finished ones while the rest exceed maxMessageBytes, and compacts
// messages.jsonl when most of its lines are superseded, or at once after
// dropping messages for the byte budget (their records would otherwise
// come back from the file on the next start).
func (s *FileStore) PruneMessages(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruneMessagesLocked(now) || s.msgLines > 2*len(s.msgs)+1000 {
		_ = s.compactMessagesLocked()
	}
}

// pruneMessagesLocked drops messages as PruneMessages does, and reports
// whether it dropped any for the byte budget.
func (s *FileStore) pruneMessagesLocked(now time.Time) bool {
	cutoff := now.Add(-messageRetention)
	total := 0
	var done []*model.Message
	for id, m := range s.msgs {
		if prunable(m) && m.CreatedAt.Before(cutoff) {
			delete(s.msgs, id)
			continue
		}
		total += messageBytes(m)
		if prunable(m) {
			done = append(done, m)
		}
	}
	if total <= maxMessageBytes {
		return false
	}
	sort.Slice(done, func(i, j int) bool { return done[i].ID < done[j].ID })
	dropped := false
	for _, m := range done {
		if total <= maxMessageBytes {
			break
		}
		total -= messageBytes(m)
		delete(s.msgs, m.ID)
		dropped = true
	}
	return dropped
}

// compactMessagesLocked rewrites messages.jsonl with one line per message.
func (s *FileStore) compactMessagesLocked() error {
	var buf bytes.Buffer
	ids := make([]string, 0, len(s.msgs))
	for id := range s.msgs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b, _ := json.Marshal(s.msgs[id])
		buf.Write(append(b, '\n'))
	}
	if s.msgLog != nil {
		_ = s.msgLog.Close()
		s.msgLog = nil
	}
	err := atomicWrite(s.msgPath(), buf.Bytes())
	if err == nil {
		s.msgLines = len(s.msgs)
		s.msgTorn = false
	}
	if oerr := s.reopenMessagesLocked(); oerr != nil {
		return oerr
	}
	return err
}

func (s *FileStore) PutMessage(m model.Message) error {
	if m.ID == "" {
		return fmt.Errorf("message without id")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msgTorn {
		if err := s.compactMessagesLocked(); err != nil {
			return err
		}
	}
	if s.msgLog == nil { // a compaction could not reopen it
		if err := s.reopenMessagesLocked(); err != nil {
			return err
		}
	}
	_, err = writeFile(s.msgLog, b)
	if err == nil {
		err = s.msgLog.Sync()
	}
	if err != nil {
		// A partial record would glue the next one onto it: cut it off,
		// or rewrite the file before the next append.
		if terr := s.msgLog.Truncate(s.msgSize); terr != nil {
			s.msgTorn = true
		}
		return err
	}
	s.msgSize += int64(len(b))
	s.msgLines++
	cp := m
	s.msgs[m.ID] = &cp
	return nil
}

func (s *FileStore) Message(id string) (model.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.msgs[id]
	if !ok {
		return model.Message{}, false
	}
	return *m, true
}

func (s *FileStore) Messages(f MessageFilter) []model.Message {
	s.mu.Lock()
	var out []model.Message
	for _, m := range s.msgs {
		if f.Session != "" && m.FromSession != f.Session && m.ToSession != f.Session {
			continue
		}
		if f.ToUser && m.ToUser == "" {
			continue
		}
		if !f.Since.IsZero() && m.CreatedAt.Before(f.Since) {
			continue
		}
		out = append(out, *m)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

func (s *FileStore) PendingMessages() []model.Message {
	s.mu.Lock()
	var out []model.Message
	for _, m := range s.msgs {
		if m.State == model.MsgPending {
			out = append(out, *m)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *FileStore) AddDailyTokens(day string, n int64) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.daily[day] += n
	s.dirty = true
}

func (s *FileStore) DailyTokens(day string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daily[day]
}

func (s *FileStore) UpsertHost(h model.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[h.ID] = h
	s.dirty = true
	return nil
}

func (s *FileStore) DeleteHost(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosts[id]; !ok {
		return ErrNoSuchHost
	}
	delete(s.hosts, id)
	for h, t := range s.tokens {
		if t.Host == id {
			delete(s.tokens, h)
		}
	}
	s.dirty = true
	return s.saveLocked()
}

func (s *FileStore) Host(id string) (model.Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[id]
	return h, ok
}

func (s *FileStore) Hosts() []model.Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *FileStore) CreateSession(sess model.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess.ID]; ok {
		return fmt.Errorf("session %s exists", sess.ID)
	}
	if _, ok := s.retired[sess.ID]; ok {
		return fmt.Errorf("session %s: %w", sess.ID, ErrRetired)
	}
	for _, other := range s.sessions {
		if other.Path == sess.Path && !Reusable(*other) {
			return ErrPathInUse
		}
	}
	// A session deleted under this id (adopted again from its node) starts
	// a new log, never the old one.
	if s.deletedLogs[sess.ID] {
		if err := s.removeLogLocked(sess.ID); err != nil {
			return fmt.Errorf("session %s: the log of a deleted session with this id: %w", sess.ID, err)
		}
	}
	cp := sess
	if Ended(cp) && cp.EndedAt.IsZero() {
		cp.EndedAt = time.Now()
	}
	s.sessions[sess.ID] = &cp
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		// The caller is told it failed: the session must not hold its
		// path, or be saved later by the flusher.
		delete(s.sessions, sess.ID)
		return err
	}
	return nil
}

func (s *FileStore) UpdateSession(id string, fn func(*model.Session)) (model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return model.Session{}, ErrNotFound
	}
	was := Ended(*sess)
	fn(sess)
	// The coordinator's clock, whatever path ended it (a node's stop
	// event, a host removed, a hello that no longer lists it).
	switch now := Ended(*sess); {
	case now && !was:
		sess.EndedAt = time.Now()
	case !now:
		sess.EndedAt = time.Time{}
	}
	s.dirty = true
	return *sess, nil
}

// RetireSessions deletes the sessions in ids (at most retireBatch of them)
// with one save, leaving tombstones; see Store.
func (s *FileStore) RetireSessions(ids []string) ([]string, error) {
	if len(ids) > retireBatch {
		ids = ids[:retireBatch]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	removed := map[string]*model.Session{}
	for _, id := range ids {
		if sess, ok := s.sessions[id]; ok {
			removed[id] = sess
			delete(s.sessions, id)
			s.deletedLogs[id] = true
			s.retired[id] = now
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		for id, sess := range removed {
			s.sessions[id] = sess
			delete(s.deletedLogs, id)
			delete(s.retired, id)
		}
		return nil, err
	}
	out := make([]string, 0, len(removed))
	for id := range removed {
		out = append(out, id)
		_ = s.removeLogLocked(id) // retried while it fails
	}
	sort.Strings(out)
	return out, nil
}

func (s *FileStore) Retired(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.retired[id]
	return ok
}

func (s *FileStore) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	// Save the removal first, with the log marked for removal in the same
	// write: a log that cannot be removed now is removed later
	// (RemoveDeletedLogs, or the next start), never left behind.
	delete(s.sessions, id)
	s.deletedLogs[id] = true
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		// state.json still lists the session: so does memory again, with
		// its history, and a retry finds it.
		s.sessions[id] = sess
		delete(s.deletedLogs, id)
		return err
	}
	if err := s.removeLogLocked(id); err != nil {
		return fmt.Errorf("%w: %v", ErrLogPending, err)
	}
	return nil
}

// ErrLogPending is returned by DeleteSession when the session is deleted
// but its event log could not be removed yet; it is removed later.
var ErrLogPending = errors.New("session deleted; its event log will be removed later")

// errLogBusy: a deleted session's log is being read; it is removed later.
var errLogBusy = errors.New("event log in use")

// removeLogLocked removes the event log of a deleted session and, once it
// is gone, forgets that it was pending. A log a reader holds is left for
// the next try.
func (s *FileStore) removeLogLocked(id string) error {
	if l, ok := s.logs[id]; ok {
		if l.pins > 0 {
			return errLogBusy
		}
		s.closeLocked(l)
		delete(s.logs, id)
	}
	// The id is a stored session's, which only a canonical ULID can be.
	if err := os.Remove(filepath.Join(s.dir, "events", id+".jsonl")); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.deletedLogs, id)
	s.dirty = true
	return nil
}

// removeDeletedLogsLocked tries again to remove the logs of deleted
// sessions, and returns how many are still there.
func (s *FileStore) removeDeletedLogsLocked() int {
	for id := range s.deletedLogs {
		_ = s.removeLogLocked(id)
	}
	return len(s.deletedLogs)
}

// RemoveDeletedLogs tries again to remove the event logs of deleted
// sessions that could not be removed before. It returns how many remain.
func (s *FileStore) RemoveDeletedLogs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-retiredTTL)
	for id, at := range s.retired {
		if at.Before(cutoff) {
			delete(s.retired, id)
			s.dirty = true
		}
	}
	if len(s.deletedLogs) == 0 {
		return 0
	}
	return s.removeDeletedLogsLocked()
}

func (s *FileStore) Session(id string) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return model.Session{}, false
	}
	return *sess, true
}

// SessionByPath returns the session that holds path: the one live session
// on it if there is one (CreateSession allows at most one), otherwise the
// newest.
func (s *FileStore) SessionByPath(path string) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The newest live session, else the newest of any.
	var best *model.Session
	bestLive := false
	for _, sess := range s.sessions {
		if sess.Path != path {
			continue
		}
		isLive := !Reusable(*sess)
		if best == nil || (isLive && !bestLive) || (isLive == bestLive && sess.ID > best.ID) {
			best, bestLive = sess, isLive
		}
	}
	if best == nil {
		return model.Session{}, false
	}
	return *best, true
}

func (s *FileStore) Sessions() []model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, *sess)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// logFor returns a session's log, opened.
func (s *FileStore) logFor(id string) (*eventLog, error) {
	l, ok := s.logs[id]
	if !ok {
		// The ID names a file, and nodes supply it: only a canonical ULID
		// may, so no ID can leave events/ or alias another session's file.
		if !ulid.Valid(id) {
			return nil, fmt.Errorf("%w: %q", ErrBadSessionID, id)
		}
		l = &eventLog{id: id, path: filepath.Join(s.dir, "events", id+".jsonl")}
	}
	if err := s.openLocked(l); err != nil {
		return nil, err
	}
	s.logs[id] = l
	return l, nil
}

// logMark is a log's state before an append.
type logMark struct {
	size, last, usage int64
	n                 int
}

// rewindLocked cuts l back to mark m: a failed write may have left part of
// a line, and the next one would be glued onto it. If the file cannot be
// cut, it is closed, and opening it again cuts off the torn line.
func (s *FileStore) rewindLocked(l *eventLog, m logMark) {
	l.lastSeq, l.usage = m.last, m.usage
	if l.f == nil {
		return
	}
	if err := l.f.Truncate(m.size); err == nil {
		if _, err := l.f.Seek(m.size, io.SeekStart); err == nil {
			l.index, l.size = l.index[:m.n], m.size
			return
		}
	}
	s.closeLocked(l)
}

func (s *FileStore) AppendEvents(evs []model.Event) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stored []model.Event
	marks := map[*eventLog]logMark{}
	defer func() {
		for l := range marks {
			l.pins--
		}
	}()
	var err error
	for _, ev := range evs {
		if ev.SessionID == "" || ev.Seq <= 0 {
			err = fmt.Errorf("event %s: missing session or seq", ev.ID)
			break
		}
		var l *eventLog
		if l, err = s.logFor(ev.SessionID); err != nil {
			break
		}
		if ev.Seq <= l.last() {
			continue // duplicate (e.g. spool replay)
		}
		var b []byte
		if b, err = json.Marshal(ev); err != nil {
			break
		}
		b = append(b, '\n')
		if _, ok := marks[l]; !ok {
			marks[l] = logMark{size: l.size, last: l.last(), usage: l.usage, n: len(l.index)}
			l.pins++ // not closed before it is synced
		}
		if _, err = writeFile(l.f, b); err != nil {
			// This log's events from this batch are written again when
			// the node resends them.
			s.rewindLocked(l, marks[l])
			break
		}
		l.index = append(l.index, idx{ev.Seq, l.size})
		l.size += int64(len(b))
		l.lastSeq = ev.Seq
		if ev.Kind == model.KindUsage {
			l.usage = ev.Seq
		}
		stored = append(stored, ev)
	}
	// What was written before a failure is kept, once it is durable: the
	// caller acknowledges up to each session's last seq.
	for l, m := range marks {
		if l.f == nil {
			continue
		}
		if serr := l.f.Sync(); serr != nil {
			s.rewindLocked(l, m)
			if err == nil {
				err = serr
			}
		}
	}
	for l := range marks {
		if s.logCap > 0 && l.f != nil && l.size > s.logCap && l.pins == 1 {
			// Only this append holds it: no reader has offsets into the
			// file being replaced.
			if cerr := s.compactLocked(l); cerr != nil {
				logf("store: compact events of %s: %v", l.id, cerr)
			}
		}
	}
	kept := stored[:0]
	for _, ev := range stored {
		if l := s.logs[ev.SessionID]; ev.Seq <= l.last() {
			kept = append(kept, ev)
		}
	}
	for l := range marks {
		if sess := s.sessions[l.id]; sess != nil && sess.LastSeq != l.last() {
			sess.LastSeq = l.last()
			s.dirty = true
		}
	}
	return kept, err
}

// SetEventLogCap sets the size in bytes past which a session's event file
// is compacted (0 or less: never). It applies from the next append.
func (s *FileStore) SetEventLogCap(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logCap = max(n, 0)
}

// TruncatedEvent is the payload of the warning event that takes the place
// of the events a compaction dropped. Its seq is the last dropped one's:
// a reader from before it learns what is missing, one past it misses
// nothing.
type TruncatedEvent struct {
	Message string `json:"message"`
	// Through is the last seq dropped; every earlier one is gone too.
	Through int64 `json:"truncated_through"`
}

// TruncatedSource is the source of a compaction's warning event.
const TruncatedSource = "coordinator"

// compactLocked rewrites l without its oldest events, keeping the newest
// half of the cap (but never an event the session has not projected yet,
// which replayProjections would need after a crash, and always the last
// event). A warning event, with the seq of the last dropped one, comes
// first. Seqs are kept, so acks, LastSeq and readers paging with `after`
// go on as before. The new file replaces the old one atomically. l must be
// open and held by no reader (the caller's own pin aside).
func (s *FileStore) compactLocked(l *eventLog) error {
	keep := s.logCap / 2
	k := sort.Search(len(l.index), func(i int) bool { return l.size-l.index[i].off <= keep })
	if sess := s.sessions[l.id]; sess != nil {
		k = min(k, sort.Search(len(l.index), func(i int) bool { return l.index[i].seq > sess.ProjectedSeq }))
	}
	k = min(k, len(l.index)-1)
	// The last usage event is kept, ahead of the warning, when it would be
	// dropped: it needs a seq of its own, so not the warning's.
	u := -1
	if l.usage > 0 {
		if j := sort.Search(len(l.index), func(i int) bool { return l.index[i].seq >= l.usage }); j < k && l.index[j].seq == l.usage {
			if j == k-1 {
				k--
			}
			if j < k-1 {
				u = j
			}
		}
	}
	// Not worth a rewrite (each reads and writes up to half the cap) for
	// less than a quarter of it; the next append looks again.
	if k <= 0 || l.index[k].off < s.logCap/4 {
		return nil
	}
	through, from := l.index[k-1].seq, l.index[k].off
	payload, err := json.Marshal(TruncatedEvent{
		Message: fmt.Sprintf("earlier history was dropped: events up to %d were past this session's %d MiB event log cap (event_log_max_mb)", through, s.logCap>>20),
		Through: through,
	})
	if err != nil {
		return err
	}
	marker, err := json.Marshal(model.Event{ID: ulid.New(), SessionID: l.id, Seq: through, TS: time.Now().UTC(),
		Kind: model.KindWarning, Source: TruncatedSource, Payload: payload})
	if err != nil {
		return err
	}
	marker = append(marker, '\n')
	var usage []byte
	if u >= 0 {
		usage = make([]byte, l.index[u+1].off-l.index[u].off)
		if _, err := l.f.ReadAt(usage, l.index[u].off); err != nil {
			return err
		}
		if bytes.IndexByte(usage, '\n') != len(usage)-1 {
			usage = nil // damaged lines between: leave it
		}
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = writeFile(f, usage); err == nil {
		_, err = writeFile(f, marker)
	}
	if err == nil {
		_, err = io.Copy(f, io.NewSectionReader(l.f, from, l.size-from))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Windows cannot rename over an open file: close it first, and open
	// whichever file is there after.
	_ = l.f.Close()
	l.f = nil
	renamed := os.Rename(tmp, l.path)
	if renamed != nil {
		_ = os.Remove(tmp)
	} else {
		_ = syncDir(filepath.Dir(l.path))
	}
	nf, err := os.OpenFile(l.path, os.O_RDWR, 0o600)
	if err == nil {
		_, err = nf.Seek(0, io.SeekEnd)
		if err != nil {
			nf.Close()
		}
	}
	if err != nil {
		// Indexed again when next used.
		l.index = nil
		s.openLogs--
		return err
	}
	l.f = nf
	if renamed != nil {
		return renamed // the old file, as it was
	}
	head := int64(len(usage) + len(marker))
	shift := from - head
	index := make([]idx, 0, len(l.index)-k+2)
	if usage != nil {
		index = append(index, idx{l.usage, 0})
	}
	index = append(index, idx{through, int64(len(usage))})
	for _, x := range l.index[k:] {
		index = append(index, idx{x.seq, x.off - shift})
	}
	l.index, l.size = index, l.size-shift
	logf("store: events of %s: dropped those up to seq %d, past the %d MiB cap", l.id, through, s.logCap>>20)
	return nil
}

func (s *FileStore) Events(sessionID string, after int64, limit int) ([]model.Event, error) {
	s.mu.Lock()
	l, ok := s.logs[sessionID]
	if !ok || l.last() <= after {
		s.mu.Unlock()
		return nil, nil
	}
	if err := s.openLocked(l); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	i := sort.Search(len(l.index), func(i int) bool { return l.index[i].seq > after })
	if i >= len(l.index) {
		s.mu.Unlock()
		return nil, nil
	}
	start := l.index[i].off
	end := l.size
	if limit > 0 && i+limit < len(l.index) {
		end = l.index[i+limit].off
	}
	// One read returns at most maxEventsRead bytes (but always at least
	// one event): callers page with `after` until they get nothing.
	if end-start > maxEventsRead {
		k := sort.Search(len(l.index), func(k int) bool { return l.index[k].off-start > maxEventsRead })
		if k <= i+1 {
			k = i + 1
		}
		if k < len(l.index) {
			end = l.index[k].off
		}
	}
	f := l.f
	l.pins++
	s.mu.Unlock()
	// Read outside the store lock: the range is already written and
	// indexed, appends only add after it, and the pin keeps the file open.
	buf := make([]byte, end-start)
	_, err := f.ReadAt(buf, start)
	s.mu.Lock()
	l.pins--
	s.mu.Unlock()
	if err != nil && err != io.EOF {
		return nil, err
	}
	var out []model.Event
	bad, prev := 0, after
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Damage from an older version's failed write: the events on
			// either side are still good.
			bad++
			continue
		}
		if ev.Seq <= prev {
			continue // not indexed either
		}
		prev = ev.Seq
		out = append(out, ev)
	}
	if bad > 0 {
		logf("store: events of %s after %d: skipped %d unreadable lines", sessionID, after, bad)
	}
	return out, nil
}

func (s *FileStore) LastSeq(sessionID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.logs[sessionID]; ok {
		return l.last()
	}
	return 0
}

func (s *FileStore) AddToken(hash, host string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepTokensLocked(time.Now())
	s.tokens[hash] = tokenRec{Hash: hash, Host: host, Expires: expires}
	s.dirty = true
	return s.saveLocked()
}

// sweepTokensLocked forgets expired enrollment tokens. Snapshots leave
// them out already; sweeping on load and on every new token keeps them
// from piling up in memory too.
func (s *FileStore) sweepTokensLocked(now time.Time) {
	for h, t := range s.tokens {
		if now.After(t.Expires) {
			delete(s.tokens, h)
		}
	}
}

func (s *FileStore) ConsumeToken(hash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok || time.Now().After(t.Expires) {
		return "", ErrBadToken
	}
	delete(s.tokens, hash)
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return t.Host, nil
}
