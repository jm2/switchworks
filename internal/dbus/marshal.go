// SPDX-License-Identifier: GPL-3.0-or-later

// Package dbus is a small D-Bus client: enough of the wire protocol to
// connect to the session bus, call methods, export objects and emit signals.
// The Linux tray uses it to publish a StatusNotifierItem, so Switchworks keeps
// to the Go standard library.
//
// Values are plain Go values. Encoding takes a signature and accepts:
// byte (y), bool (b), int16 (n), uint16 (q), int32 or int (i), uint32 or int
// (u), int64 (x), uint64 (t), float64 (d), string (s), ObjectPath (o),
// Signature (g), Variant (v); arrays as []any, []string, []int32, []byte,
// []Variant or, for a{sv}, map[string]Variant; structs as []any; dict
// entries inside []any as DictEntry. Decoding returns the same types, with
// every array as []any (except ay, which is []byte) and every struct as
// []any.
package dbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ObjectPath is a D-Bus object path (type o).
type ObjectPath string

// Signature is a D-Bus type signature (type g).
type Signature string

// Variant is a value with its own signature (type v).
type Variant struct {
	Sig   string
	Value any
}

// DictEntry is one key/value pair of a dictionary (type {kv}).
type DictEntry struct {
	Key, Value any
}

const (
	maxArray = 32 << 20 // per the specification: 64 MiB; this client needs far less
	maxDepth = 32
)

// nextType splits the first complete type off sig, validating it as the
// specification requires: a dict entry only as an array's element, with a
// basic key and exactly one value; no empty structs; bounded nesting and
// length.
func nextType(sig string) (string, string, error) {
	if len(sig) > 255 {
		return "", "", errors.New("dbus: signature too long")
	}
	n, err := typeLen(sig, 0, false)
	if err != nil {
		return "", "", err
	}
	return sig[:n], sig[n:], nil
}

func isBasic(c byte) bool {
	switch c {
	case 'y', 'b', 'n', 'q', 'i', 'u', 'x', 't', 'd', 's', 'o', 'g', 'h':
		return true
	}
	return false
}

// typeLen returns the length of the complete type at the start of sig.
func typeLen(sig string, depth int, arrayElem bool) (int, error) {
	if sig == "" {
		return 0, errors.New("dbus: empty signature")
	}
	if depth > maxDepth {
		return 0, errors.New("dbus: signature nested too deeply")
	}
	bad := func() (int, error) { return 0, fmt.Errorf("dbus: bad signature %q", sig) }
	switch c := sig[0]; {
	case isBasic(c) || c == 'v':
		return 1, nil
	case c == 'a':
		n, err := typeLen(sig[1:], depth+1, true)
		if err != nil {
			return 0, err
		}
		return 1 + n, nil
	case c == '(':
		i := 1
		for i < len(sig) && sig[i] != ')' {
			n, err := typeLen(sig[i:], depth+1, false)
			if err != nil {
				return 0, err
			}
			i += n
		}
		if i == 1 || i >= len(sig) {
			return bad() // empty struct, or no closing parenthesis
		}
		return i + 1, nil
	case c == '{':
		if !arrayElem || len(sig) < 4 || !isBasic(sig[1]) {
			return bad()
		}
		n, err := typeLen(sig[2:], depth+1, false)
		if err != nil {
			return 0, err
		}
		if 2+n >= len(sig) || sig[2+n] != '}' {
			return bad()
		}
		return 2 + n + 1, nil
	}
	return bad()
}

// splitTypes splits a signature into its complete types.
func splitTypes(sig string) ([]string, error) {
	var out []string
	for sig != "" {
		t, rest, err := nextType(sig)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		sig = rest
	}
	return out, nil
}

func alignOf(t byte) int {
	switch t {
	case 'y', 'g', 'v':
		return 1
	case 'n', 'q':
		return 2
	case 'x', 't', 'd', '(', '{':
		return 8
	}
	return 4
}

// encoder writes little-endian messages.
type encoder struct {
	buf []byte
}

func newEncoder() *encoder { return &encoder{} }

func (e *encoder) align(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) u32(v uint32) {
	e.align(4)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

func toString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case ObjectPath:
		return string(s), true
	case Signature:
		return string(s), true
	}
	return "", false
}

func typeErr(sig string, v any) error { return fmt.Errorf("dbus: cannot encode %T as %q", v, sig) }

// encodeAll encodes values for a whole signature.
func (e *encoder) encodeAll(sig string, vals []any) error {
	types, err := splitTypes(sig)
	if err != nil {
		return err
	}
	if len(types) != len(vals) {
		return fmt.Errorf("dbus: signature %q has %d types for %d values", sig, len(types), len(vals))
	}
	for i, t := range types {
		if err := e.encode(t, vals[i], 0); err != nil {
			return err
		}
	}
	return nil
}

func (e *encoder) encode(sig string, v any, depth int) error {
	if depth > maxDepth {
		return errors.New("dbus: value nested too deeply")
	}
	switch sig[0] {
	case 'y':
		n, ok := toInt64(v)
		if !ok || n < 0 || n > 255 {
			return typeErr(sig, v)
		}
		e.buf = append(e.buf, byte(n))
	case 'b':
		b, ok := v.(bool)
		if !ok {
			return typeErr(sig, v)
		}
		var n uint32
		if b {
			n = 1
		}
		e.u32(n)
	case 'n', 'q':
		n, ok := toInt64(v)
		if !ok {
			return typeErr(sig, v)
		}
		e.align(2)
		e.buf = binary.LittleEndian.AppendUint16(e.buf, uint16(n))
	case 'i', 'u', 'h':
		n, ok := toInt64(v)
		if !ok {
			return typeErr(sig, v)
		}
		e.u32(uint32(n))
	case 'x', 't':
		var n uint64
		switch x := v.(type) {
		case uint64:
			n = x
		default:
			i, ok := toInt64(v)
			if !ok {
				return typeErr(sig, v)
			}
			n = uint64(i)
		}
		e.align(8)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, n)
	case 'd':
		f, ok := v.(float64)
		if !ok {
			return typeErr(sig, v)
		}
		e.align(8)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(f))
	case 's', 'o':
		s, ok := toString(v)
		if !ok {
			return typeErr(sig, v)
		}
		e.u32(uint32(len(s)))
		e.buf = append(e.buf, s...)
		e.buf = append(e.buf, 0)
	case 'g':
		s, ok := toString(v)
		if !ok || len(s) > 255 {
			return typeErr(sig, v)
		}
		e.buf = append(e.buf, byte(len(s)))
		e.buf = append(e.buf, s...)
		e.buf = append(e.buf, 0)
	case 'v':
		vv, ok := v.(Variant)
		if !ok {
			return typeErr(sig, v)
		}
		if t, rest, err := nextType(vv.Sig); err != nil || rest != "" || t == "" {
			return fmt.Errorf("dbus: variant signature %q is not one complete type", vv.Sig)
		}
		if err := e.encode("g", vv.Sig, depth+1); err != nil {
			return err
		}
		return e.encode(vv.Sig, vv.Value, depth+1)
	case 'a':
		return e.encodeArray(sig, v, depth)
	case '(':
		fields, ok := v.([]any)
		if !ok {
			return typeErr(sig, v)
		}
		e.align(8)
		types, err := splitTypes(sig[1 : len(sig)-1])
		if err != nil {
			return err
		}
		if len(types) != len(fields) {
			return fmt.Errorf("dbus: struct %q has %d fields, got %d", sig, len(types), len(fields))
		}
		for i, t := range types {
			if err := e.encode(t, fields[i], depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("dbus: cannot encode type %q", sig)
	}
	return nil
}

func (e *encoder) encodeArray(sig string, v any, depth int) error {
	elem := sig[1:]
	e.align(4)
	lenAt := len(e.buf)
	e.buf = append(e.buf, 0, 0, 0, 0)
	e.align(alignOf(elem[0]))
	start := len(e.buf)
	var err error
	switch a := v.(type) {
	case []byte:
		if elem != "y" {
			return typeErr(sig, v)
		}
		e.buf = append(e.buf, a...)
	case []string:
		for _, s := range a {
			if err = e.encode(elem, s, depth+1); err != nil {
				return err
			}
		}
	case []int32:
		for _, n := range a {
			if err = e.encode(elem, n, depth+1); err != nil {
				return err
			}
		}
	case []Variant:
		for _, x := range a {
			if err = e.encode(elem, x, depth+1); err != nil {
				return err
			}
		}
	case map[string]Variant:
		if elem != "{sv}" {
			return typeErr(sig, v)
		}
		keys := make([]string, 0, len(a))
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err = e.encodeEntry(elem, DictEntry{k, a[k]}, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range a {
			if elem[0] == '{' {
				de, ok := x.(DictEntry)
				if !ok {
					return typeErr(elem, x)
				}
				err = e.encodeEntry(elem, de, depth+1)
			} else {
				err = e.encode(elem, x, depth+1)
			}
			if err != nil {
				return err
			}
		}
	case nil:
	default:
		return typeErr(sig, v)
	}
	n := len(e.buf) - start
	if n > maxArray {
		return errors.New("dbus: array too large")
	}
	binary.LittleEndian.PutUint32(e.buf[lenAt:], uint32(n))
	return nil
}

func (e *encoder) encodeEntry(sig string, de DictEntry, depth int) error {
	kt, rest, err := nextType(sig[1 : len(sig)-1])
	if err != nil {
		return err
	}
	e.align(8)
	if err := e.encode(kt, de.Key, depth); err != nil {
		return err
	}
	return e.encode(rest, de.Value, depth)
}

type decoder struct {
	buf   []byte
	pos   int
	order binary.ByteOrder
}

var errShort = errors.New("dbus: message truncated")

func (d *decoder) align(n int) error {
	for d.pos%n != 0 {
		if d.pos >= len(d.buf) {
			return errShort
		}
		d.pos++
	}
	return nil
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, errShort
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *decoder) u32() (uint32, error) {
	if err := d.align(4); err != nil {
		return 0, err
	}
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return d.order.Uint32(b), nil
}

func (d *decoder) decodeAll(sig string) ([]any, error) {
	types, err := splitTypes(sig)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(types))
	for _, t := range types {
		v, err := d.decode(t, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (d *decoder) str(n int) (string, error) {
	b, err := d.take(n + 1)
	if err != nil {
		return "", err
	}
	if b[n] != 0 {
		return "", errors.New("dbus: string not terminated")
	}
	return string(b[:n]), nil
}

func (d *decoder) decode(sig string, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("dbus: value nested too deeply")
	}
	if sig == "" {
		return nil, errors.New("dbus: empty signature")
	}
	switch sig[0] {
	case 'y':
		b, err := d.take(1)
		if err != nil {
			return nil, err
		}
		return b[0], nil
	case 'b':
		n, err := d.u32()
		return n != 0, err
	case 'n', 'q':
		if err := d.align(2); err != nil {
			return nil, err
		}
		b, err := d.take(2)
		if err != nil {
			return nil, err
		}
		if sig[0] == 'n' {
			return int16(d.order.Uint16(b)), nil
		}
		return d.order.Uint16(b), nil
	case 'i':
		n, err := d.u32()
		return int32(n), err
	case 'u', 'h':
		return d.u32()
	case 'x', 't', 'd':
		if err := d.align(8); err != nil {
			return nil, err
		}
		b, err := d.take(8)
		if err != nil {
			return nil, err
		}
		n := d.order.Uint64(b)
		switch sig[0] {
		case 'x':
			return int64(n), nil
		case 'd':
			return math.Float64frombits(n), nil
		}
		return n, nil
	case 's', 'o':
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		if n > maxArray {
			return nil, errShort
		}
		s, err := d.str(int(n))
		if sig[0] == 'o' {
			return ObjectPath(s), err
		}
		return s, err
	case 'g':
		b, err := d.take(1)
		if err != nil {
			return nil, err
		}
		s, err := d.str(int(b[0]))
		return Signature(s), err
	case 'v':
		g, err := d.decode("g", depth+1)
		if err != nil {
			return nil, err
		}
		s := string(g.(Signature))
		if t, rest, err := nextType(s); err != nil || rest != "" || t == "" {
			return nil, fmt.Errorf("dbus: bad variant signature %q", s)
		}
		v, err := d.decode(s, depth+1)
		return Variant{Sig: s, Value: v}, err
	case 'a':
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		if n > maxArray {
			return nil, errors.New("dbus: array too large")
		}
		elem := sig[1:]
		if err := d.align(alignOf(elem[0])); err != nil {
			return nil, err
		}
		end := d.pos + int(n)
		if end > len(d.buf) {
			return nil, errShort
		}
		if elem == "y" {
			b, _ := d.take(int(n))
			return append([]byte(nil), b...), nil
		}
		out := []any{}
		for d.pos < end {
			start := d.pos
			var v any
			if elem[0] == '{' {
				if err := d.align(8); err != nil {
					return nil, err
				}
				kt, vt, err := nextType(elem[1 : len(elem)-1])
				if err != nil {
					return nil, err
				}
				k, err := d.decode(kt, depth+1)
				if err != nil {
					return nil, err
				}
				val, err := d.decode(vt, depth+1)
				if err != nil {
					return nil, err
				}
				v = DictEntry{Key: k, Value: val}
			} else {
				v, err = d.decode(elem, depth+1)
				if err != nil {
					return nil, err
				}
			}
			if d.pos == start {
				return nil, errors.New("dbus: array element of no size")
			}
			out = append(out, v)
		}
		if d.pos != end {
			return nil, errors.New("dbus: array length mismatch")
		}
		return out, nil
	case '(':
		if err := d.align(8); err != nil {
			return nil, err
		}
		types, err := splitTypes(sig[1 : len(sig)-1])
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(types))
		for _, t := range types {
			v, err := d.decode(t, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("dbus: cannot decode type %q", sig)
}
