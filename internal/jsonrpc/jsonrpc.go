// SPDX-License-Identifier: GPL-3.0-or-later

// Package jsonrpc is a small bidirectional JSON-RPC 2.0 peer over
// newline-delimited JSON, as spoken by `codex app-server` on stdio.
//
// Codex omits the "jsonrpc":"2.0" field on the wire; this package neither
// requires it on input nor sends it on output. Both sides may send requests:
// the server sends approval requests to the client, so a Conn handles
// incoming requests, incoming notifications and responses to its own calls.
package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Error codes used by this package and by Codex.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeOverloaded is Codex's "Server overloaded; retry later." code.
	CodeOverloaded = -32001
)

// Message is one JSON-RPC message in any of its three shapes.
type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Handler receives messages initiated by the remote side. Methods are called
// sequentially from the read loop, in arrival order, so they must not block
// for long. A request handler that needs time (e.g. waiting for a human to
// approve a command) should record the id and call Conn.Reply later.
type Handler interface {
	HandleNotification(method string, params json.RawMessage)
	HandleRequest(id json.RawMessage, method string, params json.RawMessage)
}

// Tap observes raw lines. dir is "in" or "out".
type Tap func(dir string, line []byte)

// ErrClosed is returned for calls on a closed connection.
var ErrClosed = errors.New("jsonrpc: connection closed")

// Conn is a JSON-RPC peer.
type Conn struct {
	w   io.Writer
	wmu sync.Mutex
	tap Tap

	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan *Message

	handler Handler
	version string // "2.0" when the peer requires the jsonrpc field

	done     chan struct{}
	closeErr error
	once     sync.Once
}

// NewConn starts reading from r in a goroutine and returns the connection.
// Outgoing messages omit the "jsonrpc" field, as Codex's app-server does.
func NewConn(r io.Reader, w io.Writer, h Handler, tap Tap) *Conn {
	return NewConnWith(r, w, h, tap, Options{})
}

// Options adjust a connection.
type Options struct {
	// VersionField sends "jsonrpc":"2.0" on every message (strict JSON-RPC
	// peers such as MCP clients require it).
	VersionField bool
}

// NewConnWith is NewConn with options.
func NewConnWith(r io.Reader, w io.Writer, h Handler, tap Tap, o Options) *Conn {
	c := &Conn{
		w:       w,
		tap:     tap,
		pending: make(map[int64]chan *Message),
		handler: h,
		done:    make(chan struct{}),
	}
	if o.VersionField {
		c.version = "2.0"
	}
	go c.readLoop(r)
	return c
}

// Done is closed when the read side ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection closed (io.EOF on a clean close).
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.closeErr
	default:
		return nil
	}
}

func (c *Conn) shutdown(err error) {
	c.once.Do(func() {
		c.closeErr = err
		close(c.done)
		c.pmu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pmu.Unlock()
	})
}

func (c *Conn) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if c.tap != nil {
				c.tap("in", bytes.TrimRight(line, "\r\n"))
			}
			c.dispatch(line)
		}
		if err != nil {
			c.shutdown(err)
			return
		}
	}
}

func (c *Conn) dispatch(line []byte) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		// Not JSON: ignore. (Codex logs go to stderr, but be tolerant.)
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		if c.handler != nil {
			c.handler.HandleRequest(m.ID, m.Method, m.Params)
		} else {
			_ = c.ReplyError(m.ID, CodeMethodNotFound, "no handler")
		}
	case m.Method != "":
		if c.handler != nil {
			c.handler.HandleNotification(m.Method, m.Params)
		}
	case hasID:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return // not one of ours (we only send integer ids)
		}
		c.pmu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.pmu.Unlock()
		if ok {
			ch <- &m
		}
	}
}

func (c *Conn) write(m Message) error {
	m.JSONRPC = c.version
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.tap != nil {
		c.tap("out", b)
	}
	b = append(b, '\n')
	_, err = c.w.Write(b)
	return err
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		// Codex rejects requests whose params field is missing ("missing
		// field `params`"), so send an empty object instead of omitting it.
		return json.RawMessage(`{}`), nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Call sends a request and waits for its response. If result is non-nil the
// response's result is decoded into it.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	p, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("jsonrpc: marshal %s params: %w", method, err)
	}
	id := c.nextID.Add(1)
	ch := make(chan *Message, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	if err := c.write(Message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}); err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, result); err != nil {
				return fmt.Errorf("jsonrpc: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

// CallRetry is Call with exponential backoff and jitter on CodeOverloaded.
func (c *Conn) CallRetry(ctx context.Context, method string, params any, result any) error {
	backoff := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := c.Call(ctx, method, params, result)
		var rpcErr *Error
		if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != CodeOverloaded || attempt >= 6 {
			return err
		}
		sleep := backoff + time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
}

// Notify sends a notification. A nil params omits the field.
func (c *Conn) Notify(method string, params any) error {
	m := Message{Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	return c.write(m)
}

// Reply answers a request received through Handler.HandleRequest.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(Message{ID: id, Result: b})
}

// ReplyError answers a request with an error.
func (c *Conn) ReplyError(id json.RawMessage, code int, message string) error {
	return c.write(Message{ID: id, Error: &Error{Code: code, Message: message}})
}
