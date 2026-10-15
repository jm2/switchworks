// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package service

import (
	"strings"
	"testing"
)

func TestUnitText(t *testing.T) {
	for _, role := range Roles {
		b, err := unitText(role, "/home/dev/my tools/swyd")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		want := `ExecStart="/home/dev/my tools/swyd" ` + role + " run\n"
		if !strings.Contains(s, want) {
			t.Errorf("%s unit lacks %q:\n%s", role, want, s)
		}
		if strings.Contains(s, "@SWYD@") || strings.Contains(s, "User=") {
			t.Errorf("%s unit is not a plain user unit:\n%s", role, s)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/swyd":       "/usr/bin/swyd",
		"/home/dev/a b/swyd":  `"/home/dev/a b/swyd"`,
		"/home/dev/100%/swyd": "/home/dev/100%%/swyd",
		`/home/dev/a"b/swyd`:  `"/home/dev/a\"b/swyd"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
