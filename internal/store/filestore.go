// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/ulid"
)

// FileStore keeps metadata in a JSON snapshot (state.json, rewritten
// atomically) and each session's events in an append-only JSONL file
// (events/<session-id>.jsonl) with an in-memory seq->offset index.
//
// Durability: AppendEvents fsyncs the touched event files before returning,
// because nodes drop their spool once the coordinator acknowledges. Metadata
// changes are flushed at most every 500ms (immediately for session creation
// and tokens); on startup, each session's last seq is recomputed from its
// event file, so a crash loses at most a little projected status, never
// events.
type FileStore struct {
	dir string

	mu       sync.Mutex
	hosts    map[string]model.Host
	sessions map[string]*model.Session
	tokens   map[string]tokenRec
	apiToks  map[string]APIToken // by id
	logs     map[string]*eventLog
	daily    map[string]int64
	dirty    bool

	msgs     map[string]*model.Message
	msgLog   *os.File
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
	Hosts       []model.Host     `json:"hosts"`
	Sessions    []*model.Session `json:"sessions"`
	Tokens      []tokenRec       `json:"tokens"`
	APITokens   []APIToken       `json:"api_tokens,omitempty"`
	DailyTokens map[string]int64 `json:"daily_tokens,omitempty"`
}

// maxEventsRead bounds the bytes one Events call reads.
const maxEventsRead = 16 << 20

// Messages older than this in a terminal state are dropped at open.
const messageRetention = 14 * 24 * time.Hour

type idx struct {
	seq int64
	off int64
}

type eventLog struct {
	f     *os.File
	index []idx // sorted by seq
	size  int64
}

func (l *eventLog) last() int64 {
	if len(l.index) == 0 {
		return 0
	}
	return l.index[len(l.index)-1].seq
}

// OpenFileStore opens or creates a store in dir.
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		return nil, err
	}
	s := &FileStore{
		dir: dir, hosts: map[string]model.Host{}, sessions: map[string]*model.Session{},
		tokens: map[string]tokenRec{}, apiToks: map[string]APIToken{}, logs: map[string]*eventLog{}, daily: map[string]int64{},
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
			s.sessions[sess.ID] = sess
		}
		for _, t := range snap.Tokens {
			s.tokens[t.Hash] = t
		}
		for _, t := range snap.APITokens {
			s.apiToks[t.ID] = t
		}
		for d, n := range snap.DailyTokens {
			s.daily[d] = n
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := s.openMessages(); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	// Rebuild event indexes and reconcile last seq.
	entries, err := os.ReadDir(filepath.Join(dir, "events"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".jsonl")]
		if !ulid.Valid(id) {
			continue
		}
		l, err := openLog(filepath.Join(dir, "events", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("events %s: %w", id, err)
		}
		s.logs[id] = l
		if sess := s.sessions[id]; sess != nil {
			sess.LastSeq = l.last()
		}
	}
	go s.flusher()
	return s, nil
}

func openLog(path string) (*eventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &eventLog{f: f}
	br := bufio.NewReaderSize(f, 1<<20)
	var off int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var ev struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(line, &ev) == nil && ev.Seq > l.last() {
				l.index = append(l.index, idx{ev.Seq, off})
			}
			off += int64(len(line))
		}
		if err != nil {
			// A torn trailing line (crash mid-write) is cut off.
			if err := f.Truncate(off); err != nil {
				return nil, err
			}
			break
		}
	}
	l.size = off
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *FileStore) flusher() {
	defer close(s.done)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.mu.Lock()
			if s.dirty {
				_ = s.saveLocked()
			}
			s.mu.Unlock()
		case <-s.stop:
			return
		}
	}
}

func (s *FileStore) saveLocked() error {
	snap := snapshot{}
	for _, h := range s.hosts {
		snap.Hosts = append(snap.Hosts, h)
	}
	sort.Slice(snap.Hosts, func(i, j int) bool { return snap.Hosts[i].ID < snap.Hosts[j].ID })
	for _, sess := range s.sessions {
		snap.Sessions = append(snap.Sessions, sess)
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
	b, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(s.dir, "state.json"), b); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
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
	return os.Rename(tmp, path)
}

// Close flushes metadata and closes files.
func (s *FileStore) Close() error {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.saveLocked()
	for _, l := range s.logs {
		_ = l.f.Close()
	}
	if s.msgLog != nil {
		_ = s.msgLog.Close()
	}
	return err
}

// --- messages: messages.jsonl, one full record per line, last one wins ---

func (s *FileStore) msgPath() string { return filepath.Join(s.dir, "messages.jsonl") }

func (s *FileStore) openMessages() error {
	if f, err := os.Open(s.msgPath()); err == nil {
		br := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 && line[len(line)-1] == '\n' {
				var m model.Message
				if json.Unmarshal(line, &m) == nil && m.ID != "" {
					cp := m
					s.msgs[m.ID] = &cp
					s.msgLines++
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
	cutoff := time.Now().Add(-messageRetention)
	for id, m := range s.msgs {
		if m.Terminal() && m.CreatedAt.Before(cutoff) {
			delete(s.msgs, id)
		}
	}
	// Compact: rewrite one line per live message.
	if s.msgLines > len(s.msgs) {
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
		if err := atomicWrite(s.msgPath(), buf.Bytes()); err != nil {
			return err
		}
		s.msgLines = len(s.msgs)
	}
	f, err := os.OpenFile(s.msgPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.msgLog = f
	return nil
}

// maxMessageBytes bounds the text of stored messages: past it, the oldest
// finished messages go first, however recent.
const maxMessageBytes = 256 << 20

// PruneMessages drops finished messages older than the retention, then the
// oldest finished ones while their text exceeds maxMessageBytes, and
// compacts messages.jsonl when most of its lines are superseded.
func (s *FileStore) PruneMessages(now time.Time) {
	cutoff := now.Add(-messageRetention)
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	var done []*model.Message
	for id, m := range s.msgs {
		if m.Terminal() && m.CreatedAt.Before(cutoff) {
			delete(s.msgs, id)
			continue
		}
		total += len(m.Text)
		if m.Terminal() {
			done = append(done, m)
		}
	}
	if total > maxMessageBytes {
		sort.Slice(done, func(i, j int) bool { return done[i].ID < done[j].ID })
		for _, m := range done {
			if total <= maxMessageBytes {
				break
			}
			total -= len(m.Text)
			delete(s.msgs, m.ID)
		}
	}
	if s.msgLines > 2*len(s.msgs)+1000 {
		_ = s.compactMessagesLocked()
	}
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
	}
	f, oerr := os.OpenFile(s.msgPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if oerr != nil {
		return oerr
	}
	s.msgLog = f
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msgLog == nil { // a compaction could not reopen it
		f, err := os.OpenFile(s.msgPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		s.msgLog = f
	}
	if _, err := s.msgLog.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.msgLog.Sync(); err != nil {
		return err
	}
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
	for _, other := range s.sessions {
		if other.Path == sess.Path && !Reusable(*other) {
			return ErrPathInUse
		}
	}
	cp := sess
	s.sessions[sess.ID] = &cp
	s.dirty = true
	return s.saveLocked()
}

func (s *FileStore) UpdateSession(id string, fn func(*model.Session)) (model.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return model.Session{}, ErrNotFound
	}
	fn(sess)
	s.dirty = true
	return *sess, nil
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

func (s *FileStore) logFor(id string) (*eventLog, error) {
	if l, ok := s.logs[id]; ok {
		return l, nil
	}
	// The ID names a file, and nodes supply it: only a canonical ULID may,
	// so no ID can leave events/ or alias another session's file.
	if !ulid.Valid(id) {
		return nil, fmt.Errorf("%w: %q", ErrBadSessionID, id)
	}
	l, err := openLog(filepath.Join(s.dir, "events", id+".jsonl"))
	if err != nil {
		return nil, err
	}
	s.logs[id] = l
	return l, nil
}

func (s *FileStore) AppendEvents(evs []model.Event) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stored []model.Event
	touched := map[*eventLog]bool{}
	for _, ev := range evs {
		if ev.SessionID == "" || ev.Seq <= 0 {
			return stored, fmt.Errorf("event %s: missing session or seq", ev.ID)
		}
		l, err := s.logFor(ev.SessionID)
		if err != nil {
			return stored, err
		}
		if ev.Seq <= l.last() {
			continue // duplicate (e.g. spool replay)
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return stored, err
		}
		b = append(b, '\n')
		n, err := l.f.Write(b)
		if err != nil {
			return stored, err
		}
		l.index = append(l.index, idx{ev.Seq, l.size})
		l.size += int64(n)
		touched[l] = true
		stored = append(stored, ev)
		if sess := s.sessions[ev.SessionID]; sess != nil {
			sess.LastSeq = ev.Seq
			s.dirty = true
		}
	}
	for l := range touched {
		if err := l.f.Sync(); err != nil {
			return stored, err
		}
	}
	return stored, nil
}

func (s *FileStore) Events(sessionID string, after int64, limit int) ([]model.Event, error) {
	s.mu.Lock()
	l, ok := s.logs[sessionID]
	if !ok {
		s.mu.Unlock()
		return nil, nil
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
	s.mu.Unlock()
	// Read outside the store lock: the range is already written and
	// indexed, and appends only add after it.
	buf := make([]byte, end-start)
	_, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	var out []model.Event
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return out, err
		}
		out = append(out, ev)
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
	s.tokens[hash] = tokenRec{Hash: hash, Host: host, Expires: expires}
	s.dirty = true
	return s.saveLocked()
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
