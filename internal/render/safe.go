// SPDX-License-Identifier: GPL-3.0-or-later

package render

import (
	"fmt"
	"io"
	"unicode/utf8"
)

// Safe wraps a terminal writer so text from agents and models cannot
// control the terminal or disguise itself: it drops escape and other
// control characters (keeping tab and newline), including the C1 controls
// such as U+009B (CSI), and the bidirectional overrides and isolates
// (U+202A–U+202E, U+2066–U+2069) that can make text read differently from
// what it is. Without it, an agent's output could rewrite what
// `swk attach` shows, set the window title or write to the clipboard.
func Safe(w io.Writer) io.Writer { return &safeWriter{w: w} }

type safeWriter struct {
	w       io.Writer
	pending []byte // the start of a sequence split across writes
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
	case c == 0xc2:
		if len(b) < 2 {
			return 0, false, true
		}
		return 2, b[1] >= 0x80 && b[1] <= 0x9f, false
	case c == 0xe2:
		if len(b) < 3 {
			if len(b) == 2 && b[1] != 0x80 && b[1] != 0x81 {
				return 1, false, false
			}
			return 0, false, true
		}
		bidi := (b[1] == 0x80 && b[2] >= 0xaa && b[2] <= 0xae) || (b[1] == 0x81 && b[2] >= 0xa6 && b[2] <= 0xa9)
		if bidi {
			return 3, true, false
		}
		return 1, false, false
	}
	return 1, false, false
}

func (s *safeWriter) Write(p []byte) (int, error) {
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
// It writes the C1 controls and bidirectional overrides as \u escapes,
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
