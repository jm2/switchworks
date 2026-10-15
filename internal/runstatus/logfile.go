// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
)

// maxLog is the size at which a log is moved aside to <name>.1.
const maxLog = 10 << 20

// Log is an append-only log file (mode 0600) that rotates itself: when a
// write would take it past 10 MiB, the file is moved to <name>.1 (replacing
// an older one) and a new one is started, so a chatty harness or a
// reconnect loop cannot fill the disk.
type Log struct {
	path  string
	crash bool

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenLog opens path for a service that has no stderr worth writing to,
// such as swydw.exe started by a logon task. Runtime crash reports go to the
// same file.
func OpenLog(path string) (*Log, error) { return openLog(path, true) }

// OpenPlainLog is OpenLog without the crash reports, for secondary logs
// (the harnesses' stderr).
func OpenPlainLog(path string) (*Log, error) { return openLog(path, false) }

func openLog(path string, crash bool) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: path, crash: crash}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	if l.crash {
		_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	}
	return nil
}

// Write appends p, rotating first when the file would grow past the limit.
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > maxLog {
		_ = l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
	}
	// A failed reopen is retried on the next write, without rotating again
	// (which would replace the .1 just made).
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
