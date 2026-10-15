// SPDX-License-Identifier: GPL-3.0-or-later

package render

import (
	"bytes"
	"testing"
)

func TestSafe(t *testing.T) {
	var b bytes.Buffer
	w := Safe(&b)
	for _, s := range []string{"ok\tline\n", "\x1b]52;c;ZXZpbA==\x07", "\x1b[2Kspoof\r", "café \u009b31m", "\xc2", "\x9bX", " ✓\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.String(), "ok\tline\n]52;c;ZXZpbA==[2Kspoofcafé 31mX ✓\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
