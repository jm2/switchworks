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
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
