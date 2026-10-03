// SPDX-License-Identifier: GPL-3.0-or-later

// Package ulid generates ULIDs: 128-bit, lexicographically sortable
// identifiers (48-bit millisecond timestamp + 80 bits of randomness), encoded
// as 26 Crockford base32 characters. IDs from one Generator are strictly
// increasing, even within the same millisecond.
package ulid

import (
	"crypto/rand"
	"errors"
	"sync"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Generator produces monotonic ULIDs. The zero value is ready to use.
type Generator struct {
	mu     sync.Mutex
	lastMs uint64
	last   [10]byte
}

var std Generator

// New returns a new ULID string from the package-level generator.
func New() string { return std.New() }

// New returns a new ULID string.
func (g *Generator) New() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := uint64(time.Now().UnixMilli())
	if ms <= g.lastMs {
		// Same (or earlier, if the clock stepped back) millisecond: increment
		// the random part so ordering stays strict.
		ms = g.lastMs
		for i := len(g.last) - 1; i >= 0; i-- {
			g.last[i]++
			if g.last[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(g.last[:]); err != nil {
			panic("ulid: crypto/rand failed: " + err.Error())
		}
		g.lastMs = ms
	}
	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], g.last[:])
	return encode(b)
}

func encode(b [16]byte) string {
	// 128 bits -> 26 base32 chars; the first char carries only 3 bits.
	var out [26]byte
	var acc uint64
	var accBits uint
	// Process as a big-endian bit stream with 2 leading zero bits (130 bits).
	bits := make([]byte, 0, 26)
	acc = 0
	accBits = 2 // two padding zero bits at the front
	for _, x := range b {
		acc = acc<<8 | uint64(x)
		accBits += 8
		for accBits >= 5 {
			accBits -= 5
			bits = append(bits, byte(acc>>accBits)&0x1f)
		}
	}
	for i, v := range bits {
		out[i] = encoding[v]
	}
	return string(out[:])
}

// Time returns the timestamp encoded in a ULID.
func Time(id string) (time.Time, error) {
	if len(id) != 26 {
		return time.Time{}, errors.New("ulid: bad length")
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		v := decodeChar(id[i])
		if v < 0 {
			return time.Time{}, errors.New("ulid: bad character")
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)), nil
}

func decodeChar(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		c -= 'a' - 'A'
	}
	for i := 10; i < len(encoding); i++ {
		if encoding[i] == c {
			return i
		}
	}
	return -1
}
