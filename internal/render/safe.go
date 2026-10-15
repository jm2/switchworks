// SPDX-License-Identifier: GPL-3.0-or-later

package render

import "io"

// Safe wraps a terminal writer so text from agents and models cannot
// control the terminal: it drops escape and other control characters
// (keeping tab and newline), including the C1 controls such as U+009B (CSI).
// Without it, an agent's output could rewrite what `swyd attach` shows,
// set the window title or write to the clipboard.
func Safe(w io.Writer) io.Writer { return &safeWriter{w: w} }

type safeWriter struct {
	w       io.Writer
	pending bool // the previous write ended in 0xC2, a C1 control's lead byte
}

func (s *safeWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+1)
	for i := 0; i < len(p); i++ {
		c := p[i]
		if s.pending {
			s.pending = false
			if c >= 0x80 && c <= 0x9f {
				continue // U+0080..U+009F
			}
			out = append(out, 0xc2)
		}
		switch {
		case c == '\n' || c == '\t':
		case c < 0x20 || c == 0x7f:
			continue
		case c == 0xc2:
			if i+1 == len(p) {
				s.pending = true
				continue
			}
			if n := p[i+1]; n >= 0x80 && n <= 0x9f {
				i++
				continue
			}
		}
		out = append(out, c)
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
