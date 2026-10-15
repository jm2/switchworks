// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package runstatus

import (
	"os"
	"syscall"
	"unsafe"
)

var procLockFileEx = kernel32.NewProc("LockFileEx")

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errorLockViolation      = syscall.Errno(33)
)

func lockFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if err == errorLockViolation {
		return ErrLocked
	}
	return err
}
