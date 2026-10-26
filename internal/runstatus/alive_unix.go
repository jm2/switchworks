// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package runstatus

import (
	"errors"
	"syscall"
	"time"
)

// alive reports whether process pid exists. EPERM means it does, under
// another user (such as the system coordinator's account).
func alive(pid int, _ time.Time) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
