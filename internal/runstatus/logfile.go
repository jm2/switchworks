// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"os"
	"path/filepath"
	"runtime/debug"
)

// maxLog is the size at which OpenLog moves a log aside to <name>.1.
const maxLog = 10 << 20

// OpenLog opens path for appending (mode 0600, parents 0700) for a service
// that has no stderr worth writing to, such as swydw.exe started by a logon
// task. A log over 10 MiB is first moved to path.1, replacing an older one.
// Runtime crash reports go to the same file.
func OpenLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	return f, nil
}
