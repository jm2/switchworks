// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"github.com/jm2/switchyard/internal/tray"
	"github.com/jm2/switchyard/internal/tray/wintray"
	"github.com/jm2/switchyard/internal/version"
)

var errAlreadyRunning = wintray.ErrAlreadyRunning

func newBackend() (tray.Backend, error) { return wintray.New(), nil }

var procMessageBoxW = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

// reportFatal shows a message box: swyd-tray.exe is built as a GUI program
// (no console window), so there is no stderr to read.
func reportFatal(err error) {
	const mbIconError = 0x10
	title, _ := syscall.UTF16PtrFromString(version.Name)
	text, _ := syscall.UTF16PtrFromString(err.Error())
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbIconError)
}
