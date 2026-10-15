// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReporterLifecycle(t *testing.T) {
	dir := t.TempDir()
	r := Start(dir, Status{Role: "node", Host: "devbox", Coordinator: "coord.lan:7481"})
	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != "node" || s.Host != "devbox" || s.PID != os.Getpid() || s.Connected || !s.Fresh(time.Now()) {
		t.Fatalf("initial status = %+v", s)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, File))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("status file mode %v, want 0600", fi.Mode().Perm())
		}
	}
	since := time.Now().UTC().Truncate(time.Second)
	r.Update(func(s *Status) { s.Connected, s.ConnectedSince = true, since })
	s, _ = Read(dir)
	if !s.Connected || !s.ConnectedSince.Equal(since) {
		t.Fatalf("after update = %+v", s)
	}
	r.Stop()
	r.Stop() // idempotent
	if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("status file still there after Stop: %v", err)
	}
	r.Update(func(s *Status) { s.Connected = false })
	if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Update after Stop recreated the status file")
	}
	var nilR *Reporter
	nilR.Update(func(*Status) {})
	nilR.Stop()
}

func TestFresh(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		updated time.Time
		want    bool
	}{
		{time.Time{}, false},
		{now.Add(-time.Second), true},
		{now.Add(-Stale - time.Second), false},
		{now.Add(Stale + time.Second), false}, // clock skew far into the future
	} {
		if got := (Status{Updated: c.updated}).Fresh(now); got != c.want {
			t.Errorf("Fresh(updated %v) = %v, want %v", c.updated, got, c.want)
		}
	}
}

func TestStopNameStable(t *testing.T) {
	a, b := stopName(filepath.Join("home", "dev", "swyd", "node")), stopName(filepath.Join("home", "dev", "swyd", "node")+string(filepath.Separator))
	if a != b {
		t.Fatalf("stop names differ for the same dir: %q %q", a, b)
	}
	if a == stopName(filepath.Join("home", "dev", "swyd", "coordinator")) {
		t.Fatal("node and coordinator share a stop name")
	}
}

func TestLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "x.log")
	l, err := OpenPlainLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	chunk := make([]byte, 1<<20)
	for i := 0; i < 25; i++ {
		if _, err := l.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > maxLog {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
}
