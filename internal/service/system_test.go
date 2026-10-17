// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"bytes"
	"strings"
	"testing"
)

func TestSystemCommands(t *testing.T) {
	for _, cmd := range []string{"coordinator", "token", "hosts", "ls", "ui-link"} {
		if !systemCommandAllowed(cmd) {
			t.Errorf("swk --system %s refused", cmd)
		}
	}
	// Anything that would run agents (or another --system) as the
	// coordinator's account.
	for _, cmd := range []string{"", "node", "mcp", "dev", "service", "--system"} {
		if systemCommandAllowed(cmd) {
			t.Errorf("swk --system %q allowed", cmd)
		}
		var out bytes.Buffer
		if code, done := EnterSystem([]string{cmd}, &out); !done || code != 2 || !strings.Contains(out.String(), "usage") {
			t.Errorf("EnterSystem(%q) = %d, %v: %s", cmd, code, done, out.String())
		}
	}
}

func TestSystemOnlyCoordinator(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"install", "--system", "node"}, &out, &out); code != 1 || !strings.Contains(out.String(), "only the coordinator") {
		t.Fatalf("install --system node = %d: %s", code, out.String())
	}
}
