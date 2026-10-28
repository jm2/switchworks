// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
)

// shortTimeouts shrinks the call timeouts for a test.
func shortTimeouts(t *testing.T, call, margin time.Duration) {
	t.Helper()
	oc, om := callTimeout, waitMargin
	callTimeout, waitMargin = call, margin
	t.Cleanup(func() { callTimeout, waitMargin = oc, om })
}

// fleet_inbox(ticket, wait_seconds) may wait longer than an ordinary
// call, and an ask still unanswered when the wait ends is a result, not
// an error.
func TestInboxTicketWait(t *testing.T) {
	shortTimeouts(t, 200*time.Millisecond, 300*time.Millisecond)
	var waited []int
	var mu sync.Mutex
	hold := func(a fleet.InboxArgs) time.Duration { return time.Duration(a.WaitSeconds) * time.Second }
	setHold := func(f func(fleet.InboxArgs) time.Duration) {
		mu.Lock()
		hold = f
		mu.Unlock()
	}
	node := &stubNode{
		inbox: func(a fleet.InboxArgs) fleet.InboxResult {
			return fleet.InboxResult{Ticket: &fleet.AskResult{Status: fleet.AskPending, MessageID: a.Ticket}}
		},
		slow: func(a fleet.InboxArgs) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			waited = append(waited, a.WaitSeconds)
			return hold(a)
		},
	}
	c := startServer(t, node)

	// The node takes the whole wait (1 s, past the 200 ms call timeout)
	// and says pending.
	res := callTool(t, c, "fleet_inbox", map[string]any{"ticket": "ask1", "wait_seconds": 1})
	if res.IsError || !strings.Contains(res.Content[0].Text, `"status": "pending"`) ||
		!strings.Contains(res.Content[0].Text, "Not answered yet") {
		t.Fatalf("pending ticket: %+v", res)
	}

	// The node never answers: the wait still ends as pending.
	setHold(func(fleet.InboxArgs) time.Duration { return time.Hour })
	start := time.Now()
	res = callTool(t, c, "fleet_inbox", map[string]any{"ticket": "ask1", "wait_seconds": 1})
	if res.IsError || !strings.Contains(res.Content[0].Text, `"message_id": "ask1"`) ||
		!strings.Contains(res.Content[0].Text, "Not answered yet") {
		t.Fatalf("stuck ticket: %+v", res)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Fatalf("stuck ticket returned after %v, want wait + margin", d)
	}

	// The wait is capped at fleet.MaxAskWait.
	setHold(func(fleet.InboxArgs) time.Duration { return 0 })
	if res := callTool(t, c, "fleet_inbox", map[string]any{"ticket": "ask1", "wait_seconds": 99999}); res.IsError {
		t.Fatalf("capped wait: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := waited[len(waited)-1]; got != int(fleet.MaxAskWait.Seconds()) {
		t.Fatalf("wait passed on as %d", got)
	}
}

// countingListener counts the connections it has open.
type countingListener struct {
	net.Listener
	open, total atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	l.total.Add(1)
	return &countedConn{Conn: c, l: l}, nil
}

type countedConn struct {
	net.Conn
	l    *countingListener
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.l.open.Add(-1) })
	return c.Conn.Close()
}

// A node that closes a connection once it has answered one request on it
// (as one closing an idle keep-alive connection might, just as it is
// reused) still answers every call, and calls leave no connections open
// on the node: idle ones used to pile up until the node's connection cap
// refused new ones and every call failed with EOF.
func TestNodeClientConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := &countingListener{Listener: ln}
	var mu sync.Mutex
	served := map[string]bool{}
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		again := served[r.RemoteAddr]
		served[r.RemoteAddr] = true
		mu.Unlock()
		if again {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close() // EOF for the client
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{}})
	}))
	hs.Listener = cl
	hs.Config.IdleTimeout = time.Hour
	hs.Start()
	t.Cleanup(hs.Close)

	nc := &NodeClient{URL: hs.URL, Session: "sess1", Token: "tok"}
	const calls = 20
	for i := 0; i < calls; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := nc.Do(ctx, "list", fleet.ListArgs{})
		cancel()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for cl.open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := cl.open.Load(); n != 0 {
		t.Fatalf("%d connections left open on the node after %d calls", n, calls)
	}
}
