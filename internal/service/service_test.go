// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/runstatus"
)

func TestStatusFromFiles(t *testing.T) {
	queryState = func(string) (state, error) { return state{}, nil }
	t.Cleanup(func() { queryState = query })
	home := t.TempDir()
	t.Setenv("SWK_HOME", home)
	t.Setenv("SWK_CLI_CONFIG", filepath.Join(home, "cli.json"))

	o := Status()
	if o.Node.Configured || o.Coordinator.Configured || o.Node.Running || o.WebUI != "" {
		t.Fatalf("empty home: %+v", o)
	}
	if got := Describe(o.Node); !strings.HasPrefix(got, "not enrolled") {
		t.Fatalf("Describe(empty node) = %q", got)
	}

	nodeDir := filepath.Join(home, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "node.json"), []byte(`{"host":"devbox","coordinator_api":"coord.lan:7480","coordinator_node":"coord.lan:7481"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o = Status()
	if !o.Node.Configured || o.Node.Running || o.WebUI != "https://coord.lan:7480/" {
		t.Fatalf("enrolled, stopped: %+v", o)
	}

	r := runstatus.Start(nodeDir, runstatus.Status{Role: "node", Host: "devbox", Coordinator: "coord.lan:7481"})
	defer r.Stop()
	r.Update(func(s *runstatus.Status) { s.Connected = true })
	o = Status()
	if !o.Node.Running || o.Node.Status == nil || !o.Node.Status.Connected {
		t.Fatalf("running node not reported: %+v", o.Node)
	}
	if got := Describe(o.Node); !strings.HasPrefix(got, "running, connected") {
		t.Fatalf("Describe(running node) = %q", got)
	}

	// A CLI config wins over node.json, and only its address is used.
	if err := os.WriteFile(filepath.Join(home, "cli.json"), []byte(`{"api":"https://coord.tailnet.example:7480","token":"secret-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o = Status()
	if o.WebUI != "https://coord.tailnet.example:7480/" {
		t.Fatalf("web UI from cli.json = %q", o.WebUI)
	}
	b, _ := json.Marshal(o)
	if bytes.Contains(b, []byte("secret-token")) {
		t.Fatal("status JSON leaks the CLI token")
	}

	var out bytes.Buffer
	if code := Main([]string{"status", "--json"}, &out, &out); code != 0 {
		t.Fatalf("status --json exit %d: %s", code, out.String())
	}
	var back Overview
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || back.Node.Status == nil || back.Node.Status.Host != "devbox" {
		t.Fatalf("status --json round trip: %v %+v", err, back)
	}
}

func TestMainUsage(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"bogus"}, {"start", "nonsense"}, {"status", "a", "b"}} {
		out.Reset()
		if code := Main(args, &out, &out); code != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, code)
		}
	}
}
