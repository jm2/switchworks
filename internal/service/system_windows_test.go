// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

import (
	"strings"
	"testing"
)

func TestServiceCommand(t *testing.T) {
	t.Setenv("ProgramData", `C:\Program Data`)
	got := serviceCommand(`C:\Program Files\Switchworks Coordinator\swk.exe`)
	want := `"C:\Program Files\Switchworks Coordinator\swk.exe" coordinator run --windows-service --dir "C:\Program Data\Switchworks\coordinator" --log-file "C:\Program Data\Switchworks\coordinator\logs\coordinator.log"`
	if got != want {
		t.Fatalf("serviceCommand =\n%s\nwant\n%s", got, want)
	}
	if !strings.HasPrefix(SystemAccount, `NT SERVICE\`) {
		t.Fatalf("SystemAccount = %q, want a virtual account", SystemAccount)
	}
}
