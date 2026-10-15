// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestTaskXML(t *testing.T) {
	s := taskXML("node", "S-1-5-21-1-2-3-1001", `C:\Users\dev\AppData\Local\Programs\Switchyard\swydw.exe`,
		`node run --log-file "C:\Users\dev\AppData\Roaming\swyd\node\logs\node.log"`, `C:\Users\dev`)
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
	if !strings.HasSuffix(task.Command, `swydw.exe`) {
		t.Fatalf("command = %q", task.Command)
	}
	enc := utf16File(s)
	if enc[0] != 0xFF || enc[1] != 0xFE {
		t.Fatal("no UTF-16LE byte-order mark")
	}
	u := make([]uint16, (len(enc)-2)/2)
	for i := range u {
		u[i] = uint16(enc[2+2*i]) | uint16(enc[3+2*i])<<8
	}
	if string(utf16.Decode(u)) != s {
		t.Fatal("UTF-16 round trip changed the text")
	}
}
