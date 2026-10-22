// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// DiskSpace returns the bytes free (to this user) and in total on the
// volume that holds dir.
func DiskSpace(dir string) (free, total uint64, err error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, all, allFree uint64
	r, _, e := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&all)), uintptr(unsafe.Pointer(&allFree)))
	if r == 0 {
		return 0, 0, e
	}
	return avail, all, nil
}
