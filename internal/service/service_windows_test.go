// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

import (
	"encoding/xml"
	"errors"
	"strings"
	"syscall"
	"testing"
)

func TestTaskXML(t *testing.T) {
	s := taskXML("node", "S-1-5-21-1-2-3-1001", `C:\Users\dev\AppData\Local\Programs\Switchworks\swkw.exe`,
		`node run --log-file "C:\Users\dev\AppData\Roaming\swk\node\logs\node.log"`, `C:\Users\dev`)
	// The declaration says UTF-16; parse the body to prove it is well formed.
	body := s[strings.Index(s, "?>")+2:]
	var task struct {
		Principal struct {
			UserID    string `xml:"UserId"`
			LogonType string
			RunLevel  string
		} `xml:"Principals>Principal"`
		Command string `xml:"Actions>Exec>Command"`
	}
	if err := xml.Unmarshal([]byte(body), &task); err != nil {
		t.Fatal(err)
	}
	if task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" || task.Principal.UserID != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("principal = %+v", task.Principal)
	}
	if !strings.HasSuffix(task.Command, `swkw.exe`) {
		t.Fatalf("command = %q", task.Command)
	}
}

func TestHResultError(t *testing.T) {
	notFound := hresultError(0x80070002)
	if !errors.Is(notFound, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(notFound, syscall.ERROR_ACCESS_DENIED) {
		t.Fatal("0x80070002 does not match only ERROR_FILE_NOT_FOUND")
	}
	if hresult(0) != nil || hresult(1) != nil {
		t.Fatal("S_OK or S_FALSE is an error")
	}
	if err := hresult(0x80041318); err == nil || err.Error() == "" {
		t.Fatal("SCHED_E_MALFORMEDXML is not an error with a message")
	}
}

// TestQueryMissingTask asks the real Task Scheduler about a task that does
// not exist, which proves the COM calls work without changing anything.
func TestQueryMissingTask(t *testing.T) {
	st, err := query("no-such-role-" + t.Name())
	if err != nil || st.installed {
		t.Fatalf("query = %+v, %v", st, err)
	}
}

func TestUnitNameCarriesSID(t *testing.T) {
	n := unitName(Node)
	if !strings.HasPrefix(n, "Switchworks node (") || !strings.Contains(n, ", S-1-") {
		t.Fatalf("task name %q lacks the SID", n)
	}
	if legacyUnitName(Node) == n {
		t.Fatal("legacy name equals the new one")
	}
}

func TestCheckExecutableRejectsPercent(t *testing.T) {
	if err := checkExecutable(`C:\Users\dev\100%\swk.exe`); err == nil {
		t.Fatal("a path with % was accepted")
	}
}
