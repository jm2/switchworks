// SPDX-License-Identifier: GPL-3.0-or-later

package dbus

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Message types.
const (
	TypeMethodCall   = 1
	TypeMethodReturn = 2
	TypeError        = 3
	TypeSignal       = 4
)

// FlagNoReplyExpected marks a call that wants no reply.
const FlagNoReplyExpected = 0x1

const maxMessage = 16 << 20

// Message is one D-Bus message.
type Message struct {
	Type        byte
	Flags       byte
	Serial      uint32
	Path        ObjectPath
	Interface   string
	Member      string
	ErrorName   string
	ReplySerial uint32
	Destination string
	Sender      string
	Signature   string
	Body        []any
}

// Error is an error reply.
type Error struct {
	Name    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Name
	}
	return e.Name + ": " + e.Message
}

// Header field codes.
const (
	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldDestination = 6
	fieldSender      = 7
	fieldSignature   = 8
)

// marshal encodes m in little-endian byte order.
func (m *Message) marshal() ([]byte, error) {
	body := newEncoder()
	if m.Signature != "" {
		if err := body.encodeAll(m.Signature, m.Body); err != nil {
			return nil, err
		}
	} else if len(m.Body) != 0 {
		return nil, errors.New("dbus: body without a signature")
	}
	var fields []any
	add := func(code byte, sig string, v any) {
		fields = append(fields, []any{code, Variant{Sig: sig, Value: v}})
	}
	if m.Path != "" {
		add(fieldPath, "o", m.Path)
	}
	if m.Interface != "" {
		add(fieldInterface, "s", m.Interface)
	}
	if m.Member != "" {
		add(fieldMember, "s", m.Member)
	}
	if m.ErrorName != "" {
		add(fieldErrorName, "s", m.ErrorName)
	}
	if m.ReplySerial != 0 {
		add(fieldReplySerial, "u", m.ReplySerial)
	}
	if m.Destination != "" {
		add(fieldDestination, "s", m.Destination)
	}
	if m.Signature != "" {
		add(fieldSignature, "g", Signature(m.Signature))
	}
	h := newEncoder()
	h.buf = append(h.buf, 'l', m.Type, m.Flags, 1)
	h.buf = binary.LittleEndian.AppendUint32(h.buf, uint32(len(body.buf)))
	h.buf = binary.LittleEndian.AppendUint32(h.buf, m.Serial)
	if err := h.encode("a(yv)", fields, 0); err != nil {
		return nil, err
	}
	h.align(8)
	out := append(h.buf, body.buf...)
	if len(out) > maxMessage {
		return nil, errors.New("dbus: message too large")
	}
	return out, nil
}

// readMessage reads one message.
func readMessage(r io.Reader) (*Message, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return nil, err
	}
	var order binary.ByteOrder
	switch fixed[0] {
	case 'l':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return nil, fmt.Errorf("dbus: bad endianness byte %q", fixed[0])
	}
	bodyLen := order.Uint32(fixed[4:])
	fieldsLen := order.Uint32(fixed[12:])
	if bodyLen > maxMessage || fieldsLen > maxMessage {
		return nil, errors.New("dbus: message too large")
	}
	hdrLen := 16 + int(fieldsLen)
	pad := (8 - hdrLen%8) % 8
	buf := make([]byte, hdrLen+pad+int(bodyLen))
	copy(buf, fixed[:])
	if _, err := io.ReadFull(r, buf[16:]); err != nil {
		return nil, err
	}
	m := &Message{Type: fixed[1], Flags: fixed[2], Serial: order.Uint32(fixed[8:])}
	d := &decoder{buf: buf[:hdrLen], pos: 12, order: order}
	fv, err := d.decode("a(yv)", 0)
	if err != nil {
		return nil, err
	}
	for _, f := range fv.([]any) {
		st := f.([]any)
		code, v := st[0].(byte), st[1].(Variant).Value
		switch code {
		case fieldPath:
			m.Path, _ = v.(ObjectPath)
		case fieldInterface:
			m.Interface, _ = v.(string)
		case fieldMember:
			m.Member, _ = v.(string)
		case fieldErrorName:
			m.ErrorName, _ = v.(string)
		case fieldReplySerial:
			m.ReplySerial, _ = v.(uint32)
		case fieldDestination:
			m.Destination, _ = v.(string)
		case fieldSender:
			m.Sender, _ = v.(string)
		case fieldSignature:
			s, _ := v.(Signature)
			m.Signature = string(s)
		}
	}
	if m.Signature != "" {
		bd := &decoder{buf: buf[hdrLen+pad:], order: order}
		if m.Body, err = bd.decodeAll(m.Signature); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Conn is a connection to a message bus.
type Conn struct {
	c    net.Conn
	r    *bufio.Reader
	name string // unique name from Hello

	wmu    sync.Mutex
	serial atomic.Uint32

	mu      sync.Mutex
	pending map[uint32]chan *Message
	closed  bool
	err     error
	handler func(*Message)

	incoming chan *Message
	done     chan struct{}
}

// SessionBus connects to the user's session bus.
func SessionBus() (*Conn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return nil, errors.New("dbus: no session bus (DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR are unset)")
		}
		addr = "unix:path=" + filepath.Join(rt, "bus")
	}
	return Dial(addr)
}

// Dial connects to the bus at a D-Bus address, authenticates and says Hello.
func Dial(address string) (*Conn, error) {
	var lastErr error = errors.New("dbus: no usable address")
	for _, a := range strings.Split(address, ";") {
		network, sockAddr, err := parseAddress(a)
		if err != nil {
			lastErr = err
			continue
		}
		nc, err := net.DialTimeout(network, sockAddr, 5*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		c, err := newConn(nc)
		if err != nil {
			nc.Close()
			lastErr = err
			continue
		}
		return c, nil
	}
	return nil, lastErr
}

func parseAddress(a string) (network, addr string, err error) {
	transport, params, ok := strings.Cut(strings.TrimSpace(a), ":")
	if !ok || transport != "unix" {
		return "", "", fmt.Errorf("dbus: unsupported address %q", a)
	}
	for _, kv := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(kv, "=")
		v, err := unescape(v)
		if err != nil {
			return "", "", err
		}
		switch k {
		case "path":
			return "unix", v, nil
		case "abstract":
			return "unix", "@" + v, nil
		}
	}
	return "", "", fmt.Errorf("dbus: unsupported address %q", a)
}

func unescape(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+2 >= len(s) {
				return "", fmt.Errorf("dbus: bad escape in address %q", s)
			}
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("dbus: bad escape in address %q", s)
			}
			b.WriteByte(byte(n))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func newConn(nc net.Conn) (*Conn, error) {
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(nc)
	uid := hex.EncodeToString([]byte(strconv.Itoa(os.Getuid())))
	if _, err := nc.Write([]byte("\x00AUTH EXTERNAL " + uid + "\r\n")); err != nil {
		return nil, err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "OK ") {
		return nil, fmt.Errorf("dbus: authentication refused: %s", strings.TrimSpace(line))
	}
	if _, err := nc.Write([]byte("BEGIN\r\n")); err != nil {
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	c := &Conn{c: nc, r: r, pending: map[uint32]chan *Message{},
		incoming: make(chan *Message, 64), done: make(chan struct{})}
	go c.readLoop()
	go c.dispatch()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := c.Call(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", "")
	if err != nil {
		c.Close()
		return nil, err
	}
	if len(rep.Body) == 1 {
		c.name, _ = rep.Body[0].(string)
	}
	return c, nil
}

// UniqueName is the connection's bus name, such as ":1.42".
func (c *Conn) UniqueName() string { return c.name }

// Handle sets the function that receives incoming method calls and signals.
// It runs on one goroutine, in arrival order, and may call Call.
func (c *Conn) Handle(h func(*Message)) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) readLoop() {
	var err error
	for {
		var m *Message
		m, err = readMessage(c.r)
		if err != nil {
			break
		}
		switch m.Type {
		case TypeMethodReturn, TypeError:
			c.mu.Lock()
			ch := c.pending[m.ReplySerial]
			delete(c.pending, m.ReplySerial)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case TypeMethodCall, TypeSignal:
			select {
			case c.incoming <- m:
			case <-c.done:
			}
		}
	}
	c.shutdown(err)
}

func (c *Conn) dispatch() {
	for {
		select {
		case m := <-c.incoming:
			c.mu.Lock()
			h := c.handler
			c.mu.Unlock()
			if h != nil {
				h(m)
			} else if m.Type == TypeMethodCall {
				_ = c.ReplyError(m, "org.freedesktop.DBus.Error.UnknownMethod", "no handler")
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == nil {
		err = net.ErrClosed
	}
	c.err = err
	pending := c.pending
	c.pending = map[uint32]chan *Message{}
	c.mu.Unlock()
	close(c.done)
	_ = c.c.Close()
	for _, ch := range pending {
		close(ch)
	}
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.shutdown(net.ErrClosed)
	return nil
}

// Send sends m, assigning its serial.
func (c *Conn) Send(m *Message) (uint32, error) {
	m.Serial = c.serial.Add(1)
	b, err := m.marshal()
	if err != nil {
		return 0, err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.c.Write(b)
	return m.Serial, err
}

// Call calls a method and waits for its reply.
func (c *Conn) Call(ctx context.Context, dest string, path ObjectPath, iface, member, sig string, args ...any) (*Message, error) {
	m := &Message{Type: TypeMethodCall, Destination: dest, Path: path, Interface: iface, Member: member, Signature: sig, Body: args}
	ch := make(chan *Message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	m.Serial = c.serial.Add(1)
	c.pending[m.Serial] = ch
	c.mu.Unlock()
	b, err := m.marshal()
	if err == nil {
		c.wmu.Lock()
		_ = c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err = c.c.Write(b)
		c.wmu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, m.Serial)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case rep, ok := <-ch:
		if !ok {
			return nil, c.Err()
		}
		if rep.Type == TypeError {
			e := &Error{Name: rep.ErrorName}
			if len(rep.Body) > 0 {
				e.Message, _ = rep.Body[0].(string)
			}
			return rep, e
		}
		return rep, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, m.Serial)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Reply answers a method call.
func (c *Conn) Reply(call *Message, sig string, args ...any) error {
	if call.Flags&FlagNoReplyExpected != 0 {
		return nil
	}
	_, err := c.Send(&Message{Type: TypeMethodReturn, ReplySerial: call.Serial, Destination: call.Sender, Signature: sig, Body: args})
	return err
}

// ReplyError answers a method call with an error.
func (c *Conn) ReplyError(call *Message, name, text string) error {
	if call.Flags&FlagNoReplyExpected != 0 {
		return nil
	}
	_, err := c.Send(&Message{Type: TypeError, ReplySerial: call.Serial, Destination: call.Sender, ErrorName: name, Signature: "s", Body: []any{text}})
	return err
}

// Emit sends a signal.
func (c *Conn) Emit(path ObjectPath, iface, member, sig string, args ...any) error {
	_, err := c.Send(&Message{Type: TypeSignal, Path: path, Interface: iface, Member: member, Signature: sig, Body: args})
	return err
}

// Name request results.
const (
	NamePrimaryOwner = 1
	NameInQueue      = 2
	NameExists       = 3
	NameAlreadyOwner = 4
)

// RequestName asks the bus for a well-known name without queueing for it.
func (c *Conn) RequestName(ctx context.Context, name string) (uint32, error) {
	const doNotQueue = 0x4
	rep, err := c.Call(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "RequestName", "su", name, uint32(doNotQueue))
	if err != nil {
		return 0, err
	}
	if len(rep.Body) != 1 {
		return 0, errors.New("dbus: bad RequestName reply")
	}
	n, _ := rep.Body[0].(uint32)
	return n, nil
}

// AddMatch subscribes to signals matching rule.
func (c *Conn) AddMatch(ctx context.Context, rule string) error {
	_, err := c.Call(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "AddMatch", "s", rule)
	return err
}
