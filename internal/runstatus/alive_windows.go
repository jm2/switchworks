// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package runstatus

import (
	"errors"
	"syscall"
	"time"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	// startSlack allows for clock adjustments between the process starting
	// and Start recording Started.
	startSlack = 2 * time.Second
)

// alive reports whether process pid runs and is the one that wrote the
// status file: a process created after started is a later one that reuses
// the pid.
func alive(pid int, started time.Time) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Another account's process (the system coordinator's) may not let
		// this user open it, but it exists.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return false
	}
	var created, exited, kernel, user syscall.Filetime
	if started.IsZero() || syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return true
	}
	return !time.Unix(0, created.Nanoseconds()).After(started.Add(startSlack))
}
