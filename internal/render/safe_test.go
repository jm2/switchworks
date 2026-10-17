// SPDX-License-Identifier: GPL-3.0-or-later

package render

import (
	"bytes"
	"encoding/json"
	"strings"
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

func TestSafeBidi(t *testing.T) {
	var b bytes.Buffer
	w := Safe(&b)
	in := "safe \u202Eevil\u202C \u2066x\u2069 \u2014 ok"
	raw := []byte(in)
	// Byte at a time: sequences split across writes are still caught.
	for i := range raw {
		if _, err := w.Write(raw[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.String(), "safe evil x \u2014 ok"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestJSONSafe(t *testing.T) {
	in := map[string]string{"text": "a\u009bb‮c⁦d é ✓"}
	var buf bytes.Buffer
	if err := json.NewEncoder(JSONSafe(Safe(&buf))).Encode(in); err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil || out["text"] != in["text"] {
		t.Fatalf("round trip: %q %v", buf.String(), err)
	}
	if strings.ContainsAny(buf.String(), "\u009b‮⁦") {
		t.Fatalf("raw controls in output: %q", buf.String())
	}
}

func TestSafeHidden(t *testing.T) {
	var b bytes.Buffer
	w := Safe(&b)
	in := "a\u200eb\u200fc\u061cd\u200be\u2060f\ufeffg \u0627\u200c\u0644 \U0001F469\u200d\U0001F4BB"
	raw := []byte(in)
	for i := range raw {
		if _, err := w.Write(raw[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	// ZWNJ and ZWJ stay: scripts and emoji sequences need them.
	if got, want := b.String(), "abcdefg \u0627\u200c\u0644 \U0001F469\u200d\U0001F4BB"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
