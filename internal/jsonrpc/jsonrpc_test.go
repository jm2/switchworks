// SPDX-License-Identifier: GPL-3.0-or-later

package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	notes  []string
	reqs   []string
	reqIDs []json.RawMessage
}

func (r *recorder) HandleNotification(method string, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, method)
}

func (r *recorder) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, method)
	r.reqIDs = append(r.reqIDs, id)
}

// pair returns two Conns wired back to back.
func pair(t *testing.T, ha, hb Handler) (*Conn, *Conn) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a := NewConn(ar, aw, ha, nil)
	b := NewConn(br, bw, hb, nil)
	t.Cleanup(func() { aw.Close(); bw.Close() })
	return a, b
}

// echoServer answers every request with {"method": <name>} and records
// notifications.
type echoServer struct {
	recorder
	conn *Conn
}

func (e *echoServer) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "fail":
		_ = e.conn.ReplyError(id, CodeInvalidParams, "nope")
	case "overloaded":
		e.mu.Lock()
		n := len(e.reqs)
		e.reqs = append(e.reqs, method)
		e.mu.Unlock()
		if n < 2 {
			_ = e.conn.ReplyError(id, CodeOverloaded, "Server overloaded; retry later.")
			return
		}
		_ = e.conn.Reply(id, map[string]any{"ok": true})
	default:
		_ = e.conn.Reply(id, map[string]any{"method": method, "params": json.RawMessage(params)})
	}
}

func TestCallNotifyAndErrors(t *testing.T) {
	srv := &echoServer{}
	client, server := pair(t, &recorder{}, srv)
	srv.conn = server

	var out struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Call(ctx, "thread/loaded/list", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != "thread/loaded/list" || string(out.Params) != "{}" {
		t.Fatalf("nil params must be sent as {}: %+v", out)
	}

	err := client.Call(ctx, "fail", map[string]int{"x": 1}, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want invalid params error, got %v", err)
	}

	if err := client.CallRetry(ctx, "overloaded", nil, nil); err != nil {
		t.Fatalf("CallRetry should succeed after overloads: %v", err)
	}

	if err := client.Notify("initialized", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.notes)
		srv.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification not received")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerInitiatedRequestAndLateReply(t *testing.T) {
	clientSide := &recorder{}
	client, server := pair(t, clientSide, &recorder{})

	// The server asks the client for approval; the client answers later.
	done := make(chan json.RawMessage, 1)
	go func() {
		var res json.RawMessage
		err := server.Call(context.Background(), "item/commandExecution/requestApproval", map[string]string{"threadId": "t1"}, &res)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	var id json.RawMessage
	for i := 0; i < 200 && id == nil; i++ {
		clientSide.mu.Lock()
		if len(clientSide.reqIDs) > 0 {
			id = clientSide.reqIDs[0]
		}
		clientSide.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if id == nil {
		t.Fatal("request not delivered")
	}
	if err := client.Reply(id, map[string]string{"decision": "accept"}); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if string(res) != `{"decision":"accept"}` {
			t.Fatalf("bad reply %s", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply not received")
	}
}

func TestPendingCallsFailOnClose(t *testing.T) {
	r, w := io.Pipe()
	c := NewConn(r, io.Discard, nil, nil)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "x", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	w.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("want ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call hung after close")
	}
}

// A peer that stops reading does not hang callers for good: the write
// times out and closes the connection, releasing every caller, so its
// owner can restart the peer.
func TestWriteTimeoutClosesConn(t *testing.T) {
	in, _ := io.Pipe()
	_, out := io.Pipe() // nobody reads: every write blocks
	c := NewConnWith(in, out, nil, nil, Options{WriteTimeout: 100 * time.Millisecond})
	errc := make(chan error, 2)
	go func() { errc <- c.Call(context.Background(), "first", nil, nil) }()
	go func() { _, err := c.Send("second", nil); errc <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("want ErrClosed, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("call still blocked on a peer that does not read")
		}
	}
	<-c.Done()
	if !errors.Is(c.Err(), ErrWriteTimeout) {
		t.Fatalf("Err %v, want ErrWriteTimeout", c.Err())
	}
	if err := c.Notify("late", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("notify after close: %v", err)
	}
}

// A call's context bounds its wait for the write too.
func TestCallContextBoundsWrite(t *testing.T) {
	in, _ := io.Pipe()
	_, out := io.Pipe()
	c := NewConnWith(in, out, nil, nil, Options{WriteTimeout: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "x", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

// gateWriter blocks writes until open is closed, then records them.
type gateWriter struct {
	open chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (w *gateWriter) Write(b []byte) (int, error) {
	<-w.open
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}

// A call that gives up while queued behind a slow write is never sent:
// its caller saw it fail, so the peer must not run it.
func TestCallGivenUpWhileQueuedIsNotSent(t *testing.T) {
	in, _ := io.Pipe()
	w := &gateWriter{open: make(chan struct{})}
	c := NewConnWith(in, w, nil, nil, Options{WriteTimeout: -1})
	go func() { _ = c.Notify("first", nil) }()
	time.Sleep(50 * time.Millisecond) // "first" is in Write
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "given-up", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	close(w.open)
	_ = c.Notify("last", nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		out := w.buf.String()
		w.mu.Unlock()
		if strings.Contains(out, `"last"`) {
			if strings.Contains(out, "given-up") {
				t.Fatalf("given-up call was sent: %s", out)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("writes never finished: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// replier answers every request from inside HandleRequest, as the read
// loop's own handlers do.
type replier struct {
	recorder
	conn *Conn
}

func (r *replier) HandleRequest(id json.RawMessage, method string, params json.RawMessage) {
	r.recorder.HandleRequest(id, method, params)
	_ = r.conn.Reply(id, map[string]string{"ok": method})
}

// Replies sent from the read loop do not stop it reading while the peer
// is slow to read them; and what was queued before the input ended is
// still written.
func TestReadLoopRepliesDoNotBlockReading(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &replier{}
	c := NewConnWith(inR, outW, h, nil, Options{WriteTimeout: -1})
	h.conn = c // before any input arrives
	feed := make(chan error, 1)
	go func() {
		_, err := io.WriteString(inW, `{"id":1,"method":"a"}`+"\n"+`{"id":2,"method":"b"}`+"\n"+`{"method":"note"}`+"\n")
		feed <- err
	}()
	// Nothing reads the replies yet, but the read loop gets to the
	// notification behind them.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.notes)
		h.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read loop blocked on its own replies")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-feed; err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); got <- string(b) }()
	inW.Close()
	<-c.Done()
	outW.Close()
	if s := <-got; s != `{"id":1,"result":{"ok":"a"}}`+"\n"+`{"id":2,"result":{"ok":"b"}}`+"\n" {
		t.Fatalf("replies %q", s)
	}
}
