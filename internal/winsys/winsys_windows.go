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

// DLL returns a lazily loaded system DLL, by its full path. If the system
// directory cannot be found (which should not happen), it falls back to the
// bare name: these are all KnownDLLs, which Windows maps from the system
// directory regardless.
func DLL(name string) *syscall.LazyDLL {
	dir, ok := systemDir()
	if !ok {
		return syscall.NewLazyDLL(name)
	}
	return syscall.NewLazyDLL(filepath.Join(dir, name))
}

func systemDir() (string, bool) {
	size := 260
	for range 4 {
		buf := make([]uint16, size)
		n, _, _ := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		switch {
		case n == 0:
			return "", false
		case int(n) < len(buf):
			return syscall.UTF16ToString(buf[:n]), true
		}
		size = int(n) + 1 // n is the size needed, with the terminator
	}
	return "", false
}
