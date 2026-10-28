// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// A status file left by a process that has exited reads as stopped however
// fresh it is; one from this process reads as running.
func TestRunning(t *testing.T) {
	now := time.Now()
	// Started as Start records it: after the process was created (on
	// Windows, a process created later than Started is another one).
	self := Status{PID: os.Getpid(), Started: now, Updated: now}
	if !self.Running(now) {
		t.Fatal("this process does not count as running")
	}
	if (Status{PID: os.Getpid(), Updated: now.Add(-Stale - time.Second)}).Running(now) {
		t.Fatal("a stale status file counts as running")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	dead := Status{Role: "node", PID: cmd.Process.Pid, Started: started, Updated: time.Now()}
	if dead.Running(time.Now()) {
		t.Fatalf("exited process %d counts as running", dead.PID)
	}
	if (Status{Updated: now}).Running(now) {
		t.Fatal("a status file without a pid counts as running")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{"role":"node","pid":`+strconv.Itoa(dead.PID)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	RemoveDead(dir)
	if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("RemoveDead kept a dead process's status file: %v", err)
	}
	r := Start(dir, Status{Role: "node"})
	defer r.Stop()
	RemoveDead(dir)
	if _, err := Read(dir); err != nil {
		t.Fatalf("RemoveDead removed a running process's status file: %v", err)
	}
}

func TestStopNameStable(t *testing.T) {
	a, b := stopName(filepath.Join("home", "dev", "swk", "node")), stopName(filepath.Join("home", "dev", "swk", "node")+string(filepath.Separator))
	if a != b {
		t.Fatalf("stop names differ for the same dir: %q %q", a, b)
	}
	if a == stopName(filepath.Join("home", "dev", "swk", "coordinator")) {
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

func TestLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	l.Close()
	l2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l2.Close()
}

// LockWait and WaitUnlocked wait for the holder to let go, as a restart
// must for the old process to exit.
func TestLockWait(t *testing.T) {
	dir := t.TempDir()
	if !WaitUnlocked(dir, 0) {
		t.Fatal("no lock file: not free")
	}
	l, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if WaitUnlocked(dir, 300*time.Millisecond) {
		t.Fatal("held lock reported free")
	}
	if _, err := LockWait(dir, 300*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock while held: %v", err)
	}
	release := 500 * time.Millisecond
	time.AfterFunc(release, func() { l.Close() })
	start := time.Now()
	if !WaitUnlocked(dir, 10*time.Second) {
		t.Fatal("released lock not seen")
	}
	if d := time.Since(start); d < release-50*time.Millisecond {
		t.Fatalf("free after %v, before the release", d)
	}
	// Probing left it free.
	l2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after probe: %v", err)
	}
	time.AfterFunc(release, func() { l2.Close() })
	l3, err := LockWait(dir, 10*time.Second)
	if err != nil {
		t.Fatalf("LockWait: %v", err)
	}
	l3.Close()
}
