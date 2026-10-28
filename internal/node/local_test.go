// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A connection over the cap is closed at once (the caller gets EOF), and
// the node says so in its log.
func TestCapListenerLogs(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var log logLines
	ln := capListener(raw, 1, log.logf)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open: the cap stays full
		}
	}()
	first, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	time.Sleep(50 * time.Millisecond)
	second, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("over the cap: read %v, want EOF", err)
	}
	// The log follows the close.
	for deadline := time.Now().Add(2 * time.Second); !log.has("connections open, the most allowed"); {
		if time.Now().After(deadline) {
			t.Fatal("no log of the refused connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A local API request that fails is logged on the node.
func TestLocalFailureLogged(t *testing.T) {
	var log logLines
	n := &Node{logf: log.logf, local: &localAPI{secret: []byte("secret")}}
	req := httptest.NewRequest("POST", "/v1/fleet/list", strings.NewReader("{}"))
	req.Header.Set("X-Swk-Session", "sess1")
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	n.handleLocal(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if !log.has(`/v1/fleet/list (session "sess1"): HTTP 401: invalid session credential`) {
		t.Fatalf("not logged: %q", log.lines)
	}
}
