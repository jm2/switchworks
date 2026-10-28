// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
)

// Fleet locks (fleet_lock): leases on named resources agents share, such as
// a host's GPU, a LAN port or a whole test host, so they need not message
// each other to ask whether something is free. Names are free text, one
// namespace for the fleet. A lease ends at its TTL, when its holder unlocks
// it, when its holder's session stops or is removed, or when an owner
// breaks it; it then goes to the longest waiting fleet_lock, if any.
//
// The table lives in memory: a coordinator restart drops every lease, and
// a holder that locks again takes it back if no one else has.

// Bounds on what agents can make the coordinator keep.
const (
	maxLocksPerSession = 32
	maxLockWaiters     = 64 // per lock
	maxLocks           = 4096
)

type lockHolder struct{ id, path string }

// lockGrant is what a waiting fleet_lock is woken with.
type lockGrant struct {
	info fleet.LockInfo
	err  error
}

type lockWaiter struct {
	who  lockHolder
	ttl  time.Duration
	note string
	ch   chan lockGrant // buffered: written once, under the table's lock
}

type heldLock struct {
	name    string
	lease   string
	who     lockHolder
	note    string
	since   time.Time
	expires time.Time
	timer   *time.Timer
	waiters []*lockWaiter // FIFO
}

type lockTable struct {
	mu    sync.Mutex
	locks map[string]*heldLock
	// alive reports whether a session may hold a lock (it is live).
	alive func(sessionID string) bool
	// broken tells a holder its lock was taken from it.
	broken func(holder lockHolder, text string)
}

// lockTab returns the server's lock table, made on first use.
func (s *Server) lockTab() *lockTable {
	s.locksOnce.Do(func() {
		s.locks = &lockTable{locks: map[string]*heldLock{},
			alive: func(id string) bool {
				sess, ok := s.store.Session(id)
				return ok && live(sess)
			},
			broken: func(h lockHolder, text string) {
				// As a system notice, so the holder stops using the
				// resource.
				s.router.notifySender(model.Message{FromSession: h.id, Kind: model.MsgNotify}, text)
			}}
	})
	return s.locks
}

func checkLockName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case len(name) > fleet.MaxLockNameBytes:
		return fmt.Errorf("name is over %d bytes", fleet.MaxLockNameBytes)
	case !utf8.ValidString(name), strings.IndexFunc(name, unicode.IsControl) >= 0:
		return errors.New("name must be text without control characters")
	case strings.TrimSpace(name) != name:
		return errors.New("name must not start or end with spaces")
	}
	return nil
}

func checkLockNote(note string) error {
	switch {
	case len(note) > fleet.MaxLockNoteBytes:
		return fmt.Errorf("note is over %d bytes", fleet.MaxLockNoteBytes)
	case !utf8.ValidString(note), strings.ContainsFunc(note, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }):
		return errors.New("note must be text without control characters")
	}
	return nil
}

// info describes l; the lease id only to its holder.
func (l *heldLock) info(holder bool) fleet.LockInfo {
	i := fleet.LockInfo{Name: l.name, Holder: l.who.path, HolderID: l.who.id, Note: l.note,
		Since: l.since, Expires: l.expires, Waiting: len(l.waiters)}
	if holder {
		i.LeaseID = l.lease
	}
	return i
}

// grant gives l to who, under t.mu.
func (t *lockTable) grant(l *heldLock, who lockHolder, ttl time.Duration, note string) {
	now := time.Now().UTC()
	l.lease, l.who, l.note, l.since, l.expires = strings.ToLower(ulid.New()), who, note, now, now.Add(ttl)
	lease := l.lease
	l.timer = time.AfterFunc(ttl, func() { t.expire(l.name, lease) })
}

// release ends l's lease, under t.mu, and passes it to the first waiter
// whose session is still live; it returns that session's path, or "" when
// none was waiting and the lock is gone.
func (t *lockTable) release(l *heldLock) string {
	l.timer.Stop()
	for len(l.waiters) > 0 {
		w := l.waiters[0]
		l.waiters = l.waiters[1:]
		if !t.alive(w.who.id) {
			w.ch <- lockGrant{err: errors.New("calling session is stopped")}
			continue
		}
		t.grant(l, w.who, w.ttl, w.note)
		w.ch <- lockGrant{info: l.info(true)}
		return w.who.path
	}
	delete(t.locks, l.name)
	return ""
}

// expire ends a lease whose TTL has passed, unless it was renewed or
// replaced since.
func (t *lockTable) expire(name, lease string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.locks[name]
	if l == nil || l.lease != lease {
		return
	}
	if d := time.Until(l.expires); d > 0 {
		l.timer.Reset(d)
		return
	}
	t.release(l)
}

func (t *lockTable) heldBy(id string) int {
	n := 0
	for _, l := range t.locks {
		if l.who.id == id {
			n++
		}
	}
	return n
}

// acquire takes name for who, or renews who's lease on it, for ttl. If
// someone else holds it, it waits up to wait (0: not at all) for it in
// FIFO order.
func (t *lockTable) acquire(ctx context.Context, who lockHolder, name string, ttl, wait time.Duration, note string) (fleet.LockResult, error) {
	t.mu.Lock()
	l := t.locks[name]
	if l != nil && !t.alive(l.who.id) {
		// Its holder ended without the release reaching us: let it go.
		t.release(l)
		l = t.locks[name]
	}
	switch {
	case l == nil:
		if len(t.locks) >= maxLocks {
			t.mu.Unlock()
			return fleet.LockResult{}, fmt.Errorf("the fleet already has %d locks", maxLocks)
		}
		if t.heldBy(who.id) >= maxLocksPerSession {
			t.mu.Unlock()
			return fleet.LockResult{}, fmt.Errorf("this session already holds %d locks; unlock some first", maxLocksPerSession)
		}
		l = &heldLock{name: name}
		t.locks[name] = l
		t.grant(l, who, ttl, note)
		defer t.mu.Unlock()
		return fleet.LockResult{Status: fleet.LockHeld, Lock: l.info(true)}, nil
	case l.who.id == who.id:
		l.expires = time.Now().UTC().Add(ttl)
		l.timer.Reset(ttl)
		if note != "" {
			l.note = note
		}
		defer t.mu.Unlock()
		return fleet.LockResult{Status: fleet.LockHeld, Renewed: true, Lock: l.info(true)}, nil
	case wait <= 0:
		defer t.mu.Unlock()
		return fleet.LockResult{Status: fleet.LockBusy, Lock: l.info(false)}, nil
	case len(l.waiters) >= maxLockWaiters:
		defer t.mu.Unlock()
		return fleet.LockResult{}, fmt.Errorf("%d sessions already wait for %q", maxLockWaiters, name)
	}
	w := &lockWaiter{who: who, ttl: ttl, note: note, ch: make(chan lockGrant, 1)}
	l.waiters = append(l.waiters, w)
	t.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g := <-w.ch:
		return heldOr(g)
	case <-timer.C:
	case <-ctx.Done():
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case g := <-w.ch:
		return heldOr(g)
	default:
	}
	l = t.locks[name]
	if l == nil {
		return fleet.LockResult{}, errors.New("lock vanished while waiting") // cannot happen: a waiter keeps it
	}
	for i, x := range l.waiters {
		if x == w {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return fleet.LockResult{}, err
	}
	return fleet.LockResult{Status: fleet.LockBusy, Lock: l.info(false)}, nil
}

func heldOr(g lockGrant) (fleet.LockResult, error) {
	if g.err != nil {
		return fleet.LockResult{}, g.err
	}
	return fleet.LockResult{Status: fleet.LockHeld, Lock: g.info}, nil
}

// unlock releases who's lease on name. Unlocking a lock no one holds is
// not an error; one someone else holds is.
func (t *lockTable) unlock(who lockHolder, name string) (fleet.UnlockResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.locks[name]
	if l == nil {
		return fleet.UnlockResult{Name: name}, nil
	}
	if l.who.id != who.id {
		return fleet.UnlockResult{}, fmt.Errorf("%q is held by %s, not by you", name, l.who.path)
	}
	return fleet.UnlockResult{Name: name, Released: true, Next: t.release(l)}, nil
}

// breakLock ends whoever's lease on name (an owner's DELETE /v1/locks) and
// tells its holder.
func (t *lockTable) breakLock(name, by string) (fleet.UnlockResult, bool) {
	t.mu.Lock()
	l := t.locks[name]
	if l == nil {
		t.mu.Unlock()
		return fleet.UnlockResult{}, false
	}
	was := l.who
	res := fleet.UnlockResult{Name: name, Released: true, Next: t.release(l)}
	t.mu.Unlock()
	t.broken(was, fmt.Sprintf("Your fleet lock %q was broken by %s: you no longer hold it. Stop using the resource, or take it again with fleet_lock.", name, by))
	return res, true
}

// releaseSession drops the leases and waits of a session that stopped or
// was removed.
func (t *lockTable) releaseSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.locks {
		kept := l.waiters[:0]
		for _, w := range l.waiters {
			if w.who.id == id {
				w.ch <- lockGrant{err: errors.New("calling session is stopped")}
			} else {
				kept = append(kept, w)
			}
		}
		l.waiters = kept
		if l.who.id == id {
			t.release(l) // deleting from the map while ranging over it is fine
		}
	}
}

func (t *lockTable) list(prefix string) []fleet.LockInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []fleet.LockInfo{}
	for name, l := range t.locks {
		if strings.HasPrefix(name, prefix) {
			out = append(out, l.info(false))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- fleet operations ---

func (s *Server) fleetLock(ctx context.Context, from sender, a fleet.LockArgs) (fleet.LockResult, error) {
	if from.sess == nil {
		return fleet.LockResult{}, errors.New("only agent sessions take locks")
	}
	if err := checkLockName(a.Name); err != nil {
		return fleet.LockResult{}, err
	}
	if err := checkLockNote(a.Note); err != nil {
		return fleet.LockResult{}, err
	}
	if a.TTLSeconds < 0 || a.WaitSeconds < 0 {
		return fleet.LockResult{}, errors.New("ttl_seconds and wait_seconds must not be negative")
	}
	ttl := fleet.DefaultLockTTL
	if a.TTLSeconds > 0 {
		ttl = min(time.Duration(a.TTLSeconds)*time.Second, fleet.MaxLockTTL)
	}
	wait := min(time.Duration(a.WaitSeconds)*time.Second, fleet.MaxLockWait)
	return s.lockTab().acquire(ctx, lockHolder{id: from.sess.ID, path: from.sess.Path}, a.Name, ttl, wait, a.Note)
}

func (s *Server) fleetUnlock(from sender, a fleet.UnlockArgs) (fleet.UnlockResult, error) {
	if from.sess == nil {
		return fleet.UnlockResult{}, errors.New("only agent sessions hold locks; an owner breaks one with DELETE /v1/locks/{name}")
	}
	if err := checkLockName(a.Name); err != nil {
		return fleet.UnlockResult{}, err
	}
	return s.lockTab().unlock(lockHolder{id: from.sess.ID, path: from.sess.Path}, a.Name)
}

// --- HTTP API ---

func (s *Server) handleLocks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, fleet.LocksResult{Locks: s.lockTab().list(r.URL.Query().Get("prefix"))})
}

// handleBreakLock ends a lease: the owner's (the admin token, an owner
// token, or any admin-scope token) to do.
func (s *Server) handleBreakLock(w http.ResponseWriter, r *http.Request) {
	via, owner, _ := s.credential(r.Context())
	if !owner && grantFrom(r.Context()).scope < scopeAdmin {
		writeErr(w, 403, errors.New("breaking a lock needs the owner's credential or admin scope"))
		return
	}
	name := r.PathValue("name")
	res, ok := s.lockTab().breakLock(name, via)
	if !ok {
		writeErr(w, 404, fmt.Errorf("no lock %q", name))
		return
	}
	s.logf("lock %q broken by %s", name, via)
	writeJSON(w, 200, res)
}
