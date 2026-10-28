// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/wire"
)

func lockAs(t *testing.T, s *Server, sess model.Session, a fleet.LockArgs) fleet.LockResult {
	t.Helper()
	res, err := s.fleetOp(context.Background(), from(sess), wire.ReqLock, model.Marshal(a))
	if err != nil {
		t.Fatalf("lock %s as %s: %v", a.Name, sess.Path, err)
	}
	return res.(fleet.LockResult)
}

func holder(l fleet.LockInfo) fleet.LockInfo {
	return fleet.LockInfo{Name: l.Name, Holder: l.Holder, HolderID: l.HolderID, Note: l.Note}
}

func TestLockAcquireBusyRelease(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	const gpu = "host:macmini-m1:gpu"

	got := lockAs(t, s, a, fleet.LockArgs{Name: gpu, Note: "training run"})
	if got.Status != fleet.LockHeld || got.Renewed || got.Lock.LeaseID == "" || got.Lock.Holder != a.Path {
		t.Fatalf("first lock: %+v", got)
	}
	if d := time.Until(got.Lock.Expires); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("default TTL: expires in %v", d)
	}
	busy := lockAs(t, s, b, fleet.LockArgs{Name: gpu})
	if busy.Status != fleet.LockBusy || busy.Lock.LeaseID != "" ||
		holder(busy.Lock) != (fleet.LockInfo{Name: gpu, Holder: a.Path, HolderID: a.ID, Note: "training run"}) ||
		!busy.Lock.Since.Equal(got.Lock.Since) || !busy.Lock.Expires.Equal(got.Lock.Expires) {
		t.Fatalf("busy: %+v", busy)
	}
	// Locking again renews, keeping the lease.
	renew := lockAs(t, s, a, fleet.LockArgs{Name: gpu, TTLSeconds: 7200})
	if renew.Status != fleet.LockHeld || !renew.Renewed || renew.Lock.LeaseID != got.Lock.LeaseID ||
		renew.Lock.Note != "training run" || time.Until(renew.Lock.Expires) < 119*time.Minute {
		t.Fatalf("renew: %+v", renew)
	}
	// A TTL over the cap is cut to it.
	renew = lockAs(t, s, a, fleet.LockArgs{Name: gpu, TTLSeconds: 1 << 30})
	if d := time.Until(renew.Lock.Expires); d > fleet.MaxLockTTL || d < fleet.MaxLockTTL-time.Minute {
		t.Fatalf("capped TTL: expires in %v", d)
	}

	if _, err := s.fleetOp(context.Background(), from(b), wire.ReqUnlock, model.Marshal(fleet.UnlockArgs{Name: gpu})); err == nil ||
		!strings.Contains(err.Error(), "held by devbox/a") {
		t.Fatalf("unlock by another session: %v", err)
	}
	locks, err := s.fleetOp(context.Background(), from(b), wire.ReqLocks, json.RawMessage(`{"prefix":"host:macmini"}`))
	if err != nil || len(locks.(fleet.LocksResult).Locks) != 1 || locks.(fleet.LocksResult).Locks[0].LeaseID != "" {
		t.Fatalf("locks: %+v %v", locks, err)
	}
	res, err := s.fleetOp(context.Background(), from(a), wire.ReqUnlock, model.Marshal(fleet.UnlockArgs{Name: gpu}))
	if err != nil || !res.(fleet.UnlockResult).Released {
		t.Fatalf("unlock: %+v %v", res, err)
	}
	if res, err := s.fleetOp(context.Background(), from(a), wire.ReqUnlock, model.Marshal(fleet.UnlockArgs{Name: gpu})); err != nil ||
		res.(fleet.UnlockResult).Released {
		t.Fatalf("unlock of a free lock: %+v %v", res, err)
	}
	if got := lockAs(t, s, b, fleet.LockArgs{Name: gpu}); got.Status != fleet.LockHeld || got.Lock.Holder != b.Path {
		t.Fatalf("lock after release: %+v", got)
	}

	// Bad arguments.
	for _, raw := range []string{`{"name":""}`, `{"name":" x"}`, `{"name":"a\u0000b"}`, `{"name":"x","ttl_seconds":-1}`,
		`{"name":"x","wait":5}`, `{"name":"` + strings.Repeat("n", fleet.MaxLockNameBytes+1) + `"}`} {
		if _, err := s.fleetOp(context.Background(), from(a), wire.ReqLock, json.RawMessage(raw)); err == nil {
			t.Errorf("lock %s: no error", raw)
		}
	}
}

func TestLockExpires(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	tab := s.lockTab()
	ctx := context.Background()
	if r, err := tab.acquire(ctx, lockHolder{a.ID, a.Path}, "port:10.0.0.5:28960", 50*time.Millisecond, 0, ""); err != nil || r.Status != fleet.LockHeld {
		t.Fatalf("lock: %+v %v", r, err)
	}
	// A waiter gets it when the lease ends.
	start := time.Now()
	r, err := tab.acquire(ctx, lockHolder{b.ID, b.Path}, "port:10.0.0.5:28960", time.Hour, 5*time.Second, "")
	if err != nil || r.Status != fleet.LockHeld || r.Lock.Holder != b.Path || time.Since(start) > 3*time.Second {
		t.Fatalf("waiter after expiry: %+v %v after %v", r, err, time.Since(start))
	}
	// With no one waiting, an expired lock is gone.
	if _, err := tab.acquire(ctx, lockHolder{a.ID, a.Path}, "short", 30*time.Millisecond, 0, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(tab.list("short")) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("expired lock still listed: %+v", tab.list("short"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waiting(s *Server, name string) int {
	for _, l := range s.lockTab().list(name) {
		if l.Name == name {
			return l.Waiting
		}
	}
	return -1
}

func TestLockWaitersFIFO(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	c := addSession(t, s, "devbox", "c")
	d := addSession(t, s, "devbox", "d")
	const host = "host:testbox"
	lockAs(t, s, a, fleet.LockArgs{Name: host})

	// A wait that ends while it is still busy says so.
	start := time.Now()
	if r := lockAs(t, s, d, fleet.LockArgs{Name: host, WaitSeconds: 1}); r.Status != fleet.LockBusy || r.Lock.Holder != a.Path ||
		time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timed-out wait: %+v after %v", r, time.Since(start))
	}
	if n := waiting(s, host); n != 0 {
		t.Fatalf("a timed-out waiter stays queued: %d", n)
	}

	got := make(chan string, 2)
	wait := func(sess model.Session, note string) {
		r, err := s.fleetOp(context.Background(), from(sess), wire.ReqLock, model.Marshal(fleet.LockArgs{Name: host, WaitSeconds: 60, Note: note}))
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		lr := r.(fleet.LockResult)
		got <- lr.Status + " " + lr.Lock.Holder + " " + lr.Lock.Note
	}
	go wait(b, "b's turn")
	waitFor(t, "b to queue", func() bool { return waiting(s, host) == 1 })
	go wait(c, "c's turn")
	waitFor(t, "c to queue", func() bool { return waiting(s, host) == 2 })

	res, err := s.fleetUnlock(from(a), fleet.UnlockArgs{Name: host})
	if err != nil || res.Next != b.Path {
		t.Fatalf("unlock: %+v %v", res, err)
	}
	if g := <-got; g != "held devbox/b b's turn" {
		t.Fatalf("first waiter: %s", g)
	}
	select {
	case g := <-got:
		t.Fatalf("second waiter got it too: %s", g)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := s.fleetUnlock(from(b), fleet.UnlockArgs{Name: host}); err != nil {
		t.Fatal(err)
	}
	if g := <-got; g != "held devbox/c c's turn" {
		t.Fatalf("second waiter: %s", g)
	}
}

func TestLockReleasedWhenSessionEnds(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	c := addSession(t, s, "devbox", "c")
	lockAs(t, s, a, fleet.LockArgs{Name: "gpu"})
	lockAs(t, s, a, fleet.LockArgs{Name: "other"})
	lockAs(t, s, c, fleet.LockArgs{Name: "cs"})

	got := make(chan fleet.LockResult, 1)
	go func() {
		r, _ := s.fleetLock(context.Background(), from(b), fleet.LockArgs{Name: "gpu", WaitSeconds: 60})
		got <- r
	}()
	waitFor(t, "b to queue", func() bool { return waiting(s, "gpu") == 1 })

	// a stops: its event moves it from live to stopped.
	before, _ := s.store.Session(a.ID)
	if _, err := s.store.UpdateSession(a.ID, func(x *model.Session) { x.Status = model.StatusStopped }); err != nil {
		t.Fatal(err)
	}
	s.projected(before, a.ID)
	select {
	case r := <-got:
		if r.Status != fleet.LockHeld || r.Lock.Holder != b.Path {
			t.Fatalf("waiter: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not get the lock of a stopped session")
	}
	if l := s.lockTab().list("other"); len(l) != 0 {
		t.Fatalf("a stopped session keeps its locks: %+v", l)
	}

	// A removed session's locks go too.
	s.forgotten(c.ID)
	if l := s.lockTab().list("cs"); len(l) != 0 {
		t.Fatalf("a removed session keeps its locks: %+v", l)
	}

	// A holder that ended without the release (an older record) loses the
	// lock to the next caller.
	lockAs(t, s, b, fleet.LockArgs{Name: "lazy"})
	if _, err := s.store.UpdateSession(b.ID, func(x *model.Session) { x.Status = model.StatusGone }); err != nil {
		t.Fatal(err)
	}
	d := addSession(t, s, "devbox", "d")
	if r := lockAs(t, s, d, fleet.LockArgs{Name: "lazy"}); r.Status != fleet.LockHeld {
		t.Fatalf("lock of an ended holder: %+v", r)
	}
}

func TestLockLimits(t *testing.T) {
	s := testServer(t, Guards{})
	a := addSession(t, s, "devbox", "a")
	for i := range maxLocksPerSession {
		lockAs(t, s, a, fleet.LockArgs{Name: "l" + string(rune('A'+i))})
	}
	if _, err := s.fleetLock(context.Background(), from(a), fleet.LockArgs{Name: "one-more"}); err == nil {
		t.Fatal("no cap on the locks one session holds")
	}
}

func TestLocksAPI(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	const name = "port:192.168.1.10:28960/udp"
	lockAs(t, s, a, fleet.LockArgs{Name: name, Note: "cod4 server"})
	got := make(chan fleet.LockResult, 1)
	go func() {
		r, _ := s.fleetLock(context.Background(), from(b), fleet.LockArgs{Name: name, WaitSeconds: 60})
		got <- r
	}()
	waitFor(t, "b to queue", func() bool { return waiting(s, name) == 1 })

	reader := e.createToken(t, "reader", "read", 0).Secret
	operator := e.createToken(t, "operator", "operate", 0).Secret
	r := e.do(t, "GET", "/v1/locks", "", map[string]string{"Authorization": "Bearer " + reader})
	var list fleet.LocksResult
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || r.StatusCode != 200 || len(list.Locks) != 1 ||
		list.Locks[0].Holder != a.Path || list.Locks[0].Note != "cod4 server" || list.Locks[0].Waiting != 1 || list.Locks[0].LeaseID != "" {
		t.Fatalf("GET /v1/locks: %d %+v %v", r.StatusCode, list, err)
	}

	path := "/v1/locks/" + url.PathEscape(name)
	for tok, want := range map[string]int{reader: 403, operator: 403} {
		if r := e.do(t, "DELETE", path, "", map[string]string{"Authorization": "Bearer " + tok}); r.StatusCode != want {
			b, _ := io.ReadAll(r.Body)
			t.Errorf("DELETE as a non-owner: %d %s, want %d", r.StatusCode, b, want)
		}
	}
	if r := e.do(t, "DELETE", "/v1/locks/nope", "", map[string]string{"Authorization": "Bearer " + e.token}); r.StatusCode != 404 {
		t.Fatalf("DELETE of a free lock: %d", r.StatusCode)
	}
	r = e.do(t, "DELETE", path, "", map[string]string{"Authorization": "Bearer " + e.token})
	var res fleet.UnlockResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil || r.StatusCode != 200 || !res.Released || res.Next != b.Path || res.Name != name {
		t.Fatalf("DELETE as the owner: %d %+v %v", r.StatusCode, res, err)
	}
	select {
	case lr := <-got:
		if lr.Status != fleet.LockHeld || lr.Lock.Holder != b.Path {
			t.Fatalf("waiter after break: %+v", lr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not get the broken lock")
	}
	// The old holder is told.
	var told bool
	for _, m := range noticesTo(s, a) {
		told = told || strings.Contains(m.Text, "was broken by admin")
	}
	if !told {
		t.Fatalf("no notice to the holder: %+v", noticesTo(s, a))
	}

	// An owner token may break one too.
	owner := e.createToken(t, "phone", "operate", 0)
	if r := e.do(t, "POST", "/v1/tokens/phone/owner", `{"owner":true}`, map[string]string{"Authorization": "Bearer " + e.token}); r.StatusCode != 200 {
		t.Fatalf("mark owner: %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", path, "", map[string]string{"Authorization": "Bearer " + owner.Secret}); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("DELETE with an owner token: %d %s", r.StatusCode, b)
	}
	if l := s.lockTab().list(""); len(l) != 0 {
		t.Fatalf("locks left: %+v", l)
	}
}
