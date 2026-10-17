// SPDX-License-Identifier: GPL-3.0-or-later

package render

import (
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

// Safe wraps a terminal writer so text from agents and models cannot
// control the terminal or disguise itself: it drops escape and other
// control characters (keeping tab and newline), including the C1 controls
// such as U+009B (CSI), and the invisible characters listed in hidden that
// can make text read differently from what it is. Without it, an agent's
// output could rewrite what `swk attach` shows, set the window title or
// write to the clipboard. It is safe for concurrent use.
func Safe(w io.Writer) io.Writer { return &safeWriter{w: w} }

type safeWriter struct {
	mu      sync.Mutex
	w       io.Writer
	pending []byte // the start of a sequence split across writes
}

// hidden reports whether r is an invisible character Safe removes:
//   - the bidirectional overrides, embeddings and isolates (U+202A–U+202E,
//     U+2066–U+2069) and the marks LRM, RLM and ALM (U+200E, U+200F,
//     U+061C), which reorder what is shown;
//   - the zero-width space (U+200B), word joiner (U+2060) and byte order
//     mark (U+FEFF), which hide a split or a join inside a word.
//
// The zero-width non-joiner and joiner (U+200C, U+200D) stay: Persian and
// Indic text needs them to render correctly, and emoji sequences such as
// family and flag-with-symbol glyphs are built with ZWJ. Neither reorders
// text, so what they could hide is limited to a word looking whole.
func hidden(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r == 0x200b, r == 0x2060, r == 0xfeff:
		return true
	}
	return false
}

// drop reports whether the bytes at the start of b form a character to
// remove, and how many bytes it has; need > 0 means b is too short to tell.
func drop(b []byte) (n int, remove bool, need bool) {
	c := b[0]
	switch {
	case c == '\n' || c == '\t':
		return 1, false, false
	case c < 0x20 || c == 0x7f:
		return 1, true, false
	case c < 0x80:
		return 1, false, false
	}
	// Only these lead bytes start a character Safe removes: C1 controls
	// (0xc2), ALM (0xd8), the U+2xxx marks (0xe2) and U+FEFF (0xef).
	switch c {
	case 0xc2, 0xd8, 0xe2, 0xef:
	default:
		return 1, false, false
	}
	if !utf8.FullRune(b) {
		return 0, false, true
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size == 1 {
		return 1, false, false
	}
	return size, (r >= 0x80 && r <= 0x9f) || hidden(r), false
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := append(s.pending, p...)
	s.pending = nil
	out := make([]byte, 0, len(buf))
	for i := 0; i < len(buf); {
		n, remove, need := drop(buf[i:])
		if need {
			s.pending = append([]byte(nil), buf[i:]...)
			break
		}
		if !remove {
			out = append(out, buf[i:i+n]...)
		}
		i += n
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// JSONSafe wraps the writer of a JSON encoder (itself usually under Safe).
// It writes the C1 controls and the hidden characters as \u escapes,
// which mean the same in JSON, so Safe has nothing to drop and the data
// comes through unchanged. Each Write must hold whole characters, as
// json.Encoder's do.
func JSONSafe(w io.Writer) io.Writer { return jsonSafe{w} }

type jsonSafe struct{ w io.Writer }

func (j jsonSafe) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		n, remove, _ := drop(p[i:])
		if remove && n > 1 {
			r, _ := utf8.DecodeRune(p[i : i+n])
			out = fmt.Appendf(out, `\u%04x`, r)
		} else {
			out = append(out, p[i:i+n]...)
		}
		i += n
	}
	if _, err := j.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
