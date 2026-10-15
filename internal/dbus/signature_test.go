// SPDX-License-Identifier: GPL-3.0-or-later

package dbus

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestSignatureValidation(t *testing.T) {
	good := map[string]string{"s": "", "a{sv}": "", "a(ii)s": "s", "aa{oa{sv}}": "", "(a{sv}as)": "", "v": ""}
	for sig, rest := range good {
		if _, r, err := nextType(sig); err != nil || r != rest {
			t.Errorf("nextType(%q) = rest %q, %v", sig, r, err)
		}
	}
	for _, sig := range []string{"", "{sv}", "a{s}", "a{vs}", "a{svv}", "()", "a()", "(i", "a", "a{sv", "z",
		strings.Repeat("a", 40) + "s", strings.Repeat("(", 40) + "i" + strings.Repeat(")", 40), strings.Repeat("s", 256)} {
		if _, _, err := nextType(sig); err == nil {
			t.Errorf("nextType(%q) accepted", sig)
		}
	}
}

// Malformed input must fail, not panic or spin, even if a bus passed it on.
func TestDecodeMalformed(t *testing.T) {
	for _, sig := range []string{"a{s}", "a()", ""} {
		d := &decoder{buf: []byte{8, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}, order: binary.LittleEndian}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("decode(%q) panicked: %v", sig, r)
				}
			}()
			if _, err := d.decode(sig, 0); err == nil {
				t.Errorf("decode(%q) accepted", sig)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("decode(%q) did not return", sig)
		}
	}
}
