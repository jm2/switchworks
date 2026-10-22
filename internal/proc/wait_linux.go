// SPDX-License-Identifier: GPL-3.0-or-later

package proc

import (
	"syscall"
	"unsafe"
)

const pPID = 1 // waitid's P_PID

// waitExited waits for process pid to exit without reaping it
// (waitid with WNOWAIT), so its pid stays taken until it is reaped. It
// reports false if it could not wait that way.
func waitExited(pid int) bool {
	var info [128]byte // siginfo_t
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])),
			syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno != syscall.EINTR {
			return errno == 0
		}
	}
}
