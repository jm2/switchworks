// SPDX-License-Identifier: GPL-3.0-or-later

// Package winsys loads Windows system DLLs by their full path in the system
// directory, so a DLL of the same name next to the program or in the
// current directory is never picked up instead.
package winsys

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

// kernel32 is a KnownDLL, which Windows always loads from the system
// directory; it is the one DLL loaded by name.
var procGetSystemDirectoryW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW")

// DLL returns a lazily loaded system DLL, by its full path.
func DLL(name string) *syscall.LazyDLL {
	return syscall.NewLazyDLL(filepath.Join(systemDir(), name))
}

func systemDir() string {
	buf := make([]uint16, 260)
	n, _, _ := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) >= len(buf) {
		return `C:\Windows\System32`
	}
	return syscall.UTF16ToString(buf[:n])
}
