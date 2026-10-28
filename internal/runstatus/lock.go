// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked means another process already runs from the directory.
var ErrLocked = errors.New("already running")

// Lock takes dir's run lock, held until Close (or the process exits), so
// two copies of the node or coordinator never share one state directory:
// they would resume the same sessions, push each other off the
// coordinator and corrupt each other's spool.
func Lock(dir string) (*os.File, error) { return LockWait(dir, 0) }

// LockWait is Lock, waiting up to d for a process that holds the lock to
// let go of it. A restart can start the new process while the old one is
// still saving its state and ending its harnesses; without the wait the
// new one failed with "already running" and nothing started it again.
func LockWait(dir string, d time.Duration) (*os.File, error) {
	deadline := time.Now().Add(d)
	for {
		f, err := lock(dir)
		if !errors.Is(err, ErrLocked) || !time.Now().Before(deadline) {
			return f, err
		}
		time.Sleep(lockPoll)
	}
}

// RestartWait is how long the node and coordinator wait at startup for an
// old process still shutting down (a restart's) to let go of the lock.
// Longer than a node's graceful stop, which ends every harness.
const RestartWait = 45 * time.Second

// lockPoll is how often LockWait and WaitUnlocked try the lock.
const lockPoll = 200 * time.Millisecond

// WaitUnlocked waits up to d for dir's run lock to be free, that is for
// the process running from dir (if any) to exit, and reports whether it
// is. It takes the lock for a moment to find out, so it must not be called
// while starting a process that takes it.
func WaitUnlocked(dir string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		f, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_RDWR, 0)
		if err != nil {
			return true // never locked here (or unreadable: nothing to wait for)
		}
		err = lockFile(f)
		if err == nil {
			_ = unlockFile(f)
		}
		f.Close()
		if !errors.Is(err, ErrLocked) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}

func lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrLocked) {
			pid := "another process"
			if s, err := Read(dir); err == nil && s.PID > 0 {
				pid = fmt.Sprintf("process %d", s.PID)
			}
			return nil, fmt.Errorf("%s: %w (%s); stop it first (swk service stop, or end that process)", dir, ErrLocked, pid)
		}
		return nil, err
	}
	return f, nil
}
