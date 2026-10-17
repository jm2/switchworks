// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package service

import (
	"strings"
	"testing"
)

func TestUnitText(t *testing.T) {
	for _, role := range Roles {
		b, err := unitText(role, "/home/dev/my tools/swk")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		want := `ExecStart="/home/dev/my tools/swk" ` + role + " run\n"
		if !strings.Contains(s, want) {
			t.Errorf("%s unit lacks %q:\n%s", role, want, s)
		}
		if strings.Contains(s, "@SWK@") || strings.Contains(s, "User=") {
			t.Errorf("%s unit is not a plain user unit:\n%s", role, s)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/swk":       "/usr/bin/swk",
		"/home/dev/a b/swk":  `"/home/dev/a b/swk"`,
		"/home/dev/100%/swk": "/home/dev/100%%/swk",
		`/home/dev/a"b/swk`:  `"/home/dev/a\"b/swk"`,
	} {
		if got, err := systemdQuote(in); err != nil || got != want {
			t.Errorf("systemdQuote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/home/dev/a\nExecStartPre=/bin/evil\n/swk", "/home/dev/a\rb/swk", "/home/dev/a\x7fb/swk"} {
		if got, err := systemdQuote(in); err == nil {
			t.Errorf("systemdQuote(%q) = %q, want an error", in, got)
		}
		if _, err := unitText(Roles[0], in); err == nil {
			t.Errorf("unitText accepted %q", in)
		}
	}
}

func TestDropInText(t *testing.T) {
	b, err := dropInText(`/home/dev/.local/bin:/home/dev/100%/bin:/home/dev/a"b\c:/usr/bin`)
	if err != nil {
		t.Fatal(err)
	}
	want := "[Service]\nEnvironment=\"PATH=/home/dev/.local/bin:/home/dev/100%%/bin:/home/dev/a\\\"b\\\\c:/usr/bin\"\n"
	if s := string(b); !strings.HasPrefix(s, generatedMarker) || !strings.HasSuffix(s, want) {
		t.Fatalf("drop-in:\n%s\nwant it to end with:\n%s", s, want)
	}
	if _, err := dropInText("/usr/bin\nExecStartPre=/bin/evil"); err == nil {
		t.Fatal("a PATH with a newline was accepted")
	}
}

func TestLoginPATH(t *testing.T) {
	t.Setenv("SHELL", "")
	t.Setenv("HOME", "/home/dev")
	t.Setenv("PATH", ".:bin:/custom/bin::/usr/bin")
	got := loginPATH("/usr/local/bin:/usr/bin")
	if got != "/custom/bin:/usr/bin:/home/dev/.local/bin:/usr/local/bin" {
		t.Fatalf("loginPATH = %q", got)
	}
}
