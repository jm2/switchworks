package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
