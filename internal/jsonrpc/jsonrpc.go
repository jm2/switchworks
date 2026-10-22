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

// ErrWriteTimeout is why a connection closed whose peer stopped reading:
// one message could not be written within the write timeout.
var ErrWriteTimeout = errors.New("jsonrpc: write timed out (the peer stopped reading)")

// DefaultWriteTimeout is how long one message may take to write before
// the connection is closed, unless Options say otherwise.
const DefaultWriteTimeout = 30 * time.Second

// Conn is a JSON-RPC peer.
//
// One goroutine writes every outgoing message, in the order they were
// sent, so no caller holds a lock while the peer is slow to read. A peer
// that stops reading altogether closes the connection after the write
// timeout (Done, Err gives ErrWriteTimeout), so whoever watches Done can
// kill and restart it, instead of every caller hanging on the pipe.
type Conn struct {
	w            io.Writer
	tap          Tap
	writeTimeout time.Duration

	qmu   sync.Mutex
	queue []outMsg
	wake  chan struct{} // a message was queued

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
	// WriteTimeout bounds the write of one message: past it the
	// connection closes with ErrWriteTimeout. Zero means
	// DefaultWriteTimeout, negative no limit.
	WriteTimeout time.Duration
}

// NewConnWith is NewConn with options.
func NewConnWith(r io.Reader, w io.Writer, h Handler, tap Tap, o Options) *Conn {
	c := &Conn{
		w:            w,
		tap:          tap,
		writeTimeout: o.WriteTimeout,
		wake:         make(chan struct{}, 1),
		pending:      make(map[int64]chan *Message),
		handler:      h,
		done:         make(chan struct{}),
	}
	if c.writeTimeout == 0 {
		c.writeTimeout = DefaultWriteTimeout
	}
	if o.VersionField {
		c.version = "2.0"
	}
	go c.writeLoop()
	go c.readLoop(r)
	return c
}

// Done is closed when the read side ends, or a write fails or times out.
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

// maxLine caps one message from the harness. A longer line is dropped, so a
// runaway harness cannot make us buffer without bound.
const maxLine = 64 << 20

func (c *Conn) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := readLine(br, maxLine)
		if errors.Is(err, errLineTooLong) {
			if c.tap != nil {
				c.tap("in", []byte(`{"switchworks":"dropped an over-long line"}`))
			}
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 {
			if c.tap != nil {
				c.tap("in", bytes.TrimRight(line, "\r\n"))
			}
			c.dispatch(line)
		}
		if err != nil {
			// Replies the handler queued before the end still go out (an
			// MCP server exits once its input ends).
			c.flush()
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

// outMsg is one queued message. sent, when set, receives the result of
// writing it; a message with no bytes is a flush marker.
type outMsg struct {
	b    []byte
	sent chan error
}

func (c *Conn) enqueue(m outMsg) {
	c.qmu.Lock()
	c.queue = append(c.queue, m)
	c.qmu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// writeLoop writes queued messages in order. What was queued before the
// read side ended is still written; a failed or timed-out write ends the
// connection, since the stream may now hold half a message.
func (c *Conn) writeLoop() {
	for {
		c.qmu.Lock()
		for len(c.queue) == 0 {
			c.qmu.Unlock()
			select {
			case <-c.wake:
			case <-c.done:
				return
			}
			c.qmu.Lock()
		}
		m := c.queue[0]
		c.queue[0] = outMsg{}
		c.queue = c.queue[1:]
		c.qmu.Unlock()
		if m.b == nil {
			m.sent <- nil
			continue
		}
		err := c.writeOne(m.b)
		if m.sent != nil {
			m.sent <- err
		}
		if err != nil {
			c.shutdown(fmt.Errorf("jsonrpc: write: %w", err))
			return
		}
	}
}

func (c *Conn) writeOne(b []byte) error {
	if c.tap != nil {
		c.tap("out", b[:len(b)-1])
	}
	if c.writeTimeout > 0 {
		// The write itself cannot be abandoned, but everyone waiting on
		// the connection is released, and its owner kills the peer.
		t := time.AfterFunc(c.writeTimeout, func() { c.shutdown(ErrWriteTimeout) })
		defer t.Stop()
	}
	_, err := c.w.Write(b)
	return err
}

// flush waits until everything queued so far is written, the connection
// fails, or the write timeout passes.
func (c *Conn) flush() {
	m := outMsg{sent: make(chan error, 1)}
	c.enqueue(m)
	limit := c.writeTimeout
	if limit <= 0 {
		limit = 5 * time.Second
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-m.sent:
	case <-c.done:
	case <-t.C:
	}
}

// write queues m. With wait it returns once m is written, or ctx ends, or
// the connection closes; without, at once. Replies and notifications do
// not wait: the read loop sends replies itself, and must keep reading
// while the peer is slow to read them, or the two can block each other on
// full pipes. Order is kept either way.
func (c *Conn) write(ctx context.Context, m Message, wait bool) error {
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
	om := outMsg{b: append(b, '\n')}
	if wait {
		om.sent = make(chan error, 1)
	}
	c.enqueue(om)
	if !wait {
		return nil
	}
	select {
	case err := <-om.sent:
		return err
	case <-ctx.Done():
		// Still written later, in order; its answer is dropped.
		return ctx.Err()
	case <-c.done:
		select {
		case err := <-om.sent:
			if err == nil {
				return ErrClosed // written, but no answer can come
			}
			return err
		default:
			return ErrClosed
		}
	}
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
	p, err := c.send(ctx, method, params)
	if err != nil {
		return err
	}
	return p.Wait(ctx, result)
}

// Pending is a request that has been written but not yet answered.
type Pending struct {
	c      *Conn
	id     int64
	method string
	ch     chan *Message
}

// Send writes a request and returns without waiting for the response, so
// a caller can order later messages (e.g. a cancel notification) after it.
// It waits for the write, which the write timeout bounds.
func (c *Conn) Send(method string, params any) (*Pending, error) {
	return c.send(context.Background(), method, params)
}

func (c *Conn) send(ctx context.Context, method string, params any) (*Pending, error) {
	p, err := marshalParams(params)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: marshal %s params: %w", method, err)
	}
	id := c.nextID.Add(1)
	ch := make(chan *Message, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	if err := c.write(ctx, Message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}, true); err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return nil, err
	}
	return &Pending{c: c, id: id, method: method, ch: ch}, nil
}

// Wait waits for the response and decodes its result into result.
func (p *Pending) Wait(ctx context.Context, result any) error {
	select {
	case m, ok := <-p.ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, result); err != nil {
				return fmt.Errorf("jsonrpc: decode %s result: %w", p.method, err)
			}
		}
		return nil
	case <-ctx.Done():
		p.c.pmu.Lock()
		delete(p.c.pending, p.id)
		p.c.pmu.Unlock()
		return ctx.Err()
	case <-p.c.done:
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

// Notify queues a notification; it is written after everything sent
// before it. A nil params omits the field.
func (c *Conn) Notify(method string, params any) error {
	m := Message{Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	return c.write(context.Background(), m, false)
}

// Reply answers a request received through Handler.HandleRequest. Like
// Notify it queues the answer and returns; a failed write ends the
// connection.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(context.Background(), Message{ID: id, Result: b}, false)
}

// ReplyError answers a request with an error.
func (c *Conn) ReplyError(id json.RawMessage, code int, message string) error {
	return c.write(context.Background(), Message{ID: id, Error: &Error{Code: code, Message: message}}, false)
}

var errLineTooLong = errors.New("jsonrpc: line too long")

// readLine reads up to and including '\n'. Past limit bytes it discards the
// rest of the line and returns errLineTooLong.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > limit {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return nil, err
			}
			return nil, errLineTooLong
		}
		buf = append(buf, frag...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return buf, err
		}
	}
}
