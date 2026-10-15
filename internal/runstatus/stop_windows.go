// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package runstatus

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procOpenEventW          = kernel32.NewProc("OpenEventW")
	procSetEvent            = kernel32.NewProc("SetEvent")
	procWaitForMultipleObjs = kernel32.NewProc("WaitForMultipleObjects")
)

const (
	eventModifyState   = 0x0002
	errorAlreadyExists = 183
	waitObject0        = 0
	infinite           = 0xFFFFFFFF
)

func stopContext(parent context.Context, dir string) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	name, err := syscall.UTF16PtrFromString(stopName(dir))
	if err != nil {
		return ctx, cancel
	}
	// Manual-reset, initially unset. The default security descriptor lets
	// only this user (and SYSTEM and administrators) open it, but only if
	// this call creates it: an event that already exists may belong to
	// someone else (or another copy of this process), so it is not used and
	// stopping falls back to signals and the service manager.
	ev, _, callErr := procCreateEventW.Call(0, 1, 0, uintptr(unsafe.Pointer(name)))
	if ev == 0 {
		return ctx, cancel
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return ctx, cancel
	}
	// A second, unnamed event, set when ctx ends, releases the waiter.
	done, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if done == 0 {
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return ctx, cancel
	}
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		handles := [2]uintptr{ev, done}
		r, _, _ := procWaitForMultipleObjs.Call(2, uintptr(unsafe.Pointer(&handles[0])), 0, infinite)
		if r == waitObject0 {
			cancel()
		}
	}()
	go func() {
		<-ctx.Done()
		_, _, _ = procSetEvent.Call(done)
		<-waited // the handles stay open until nothing waits on them
		_ = syscall.CloseHandle(syscall.Handle(ev))
		_ = syscall.CloseHandle(syscall.Handle(done))
	}()
	return ctx, cancel
}

func requestStop(dir string) error {
	name, err := syscall.UTF16PtrFromString(stopName(dir))
	if err != nil {
		return err
	}
	ev, _, callErr := procOpenEventW.Call(eventModifyState, 0, uintptr(unsafe.Pointer(name)))
	if ev == 0 {
		return callErr // usually ERROR_FILE_NOT_FOUND: nothing running from dir
	}
	defer syscall.CloseHandle(syscall.Handle(ev))
	if r, _, callErr := procSetEvent.Call(ev); r == 0 {
		return callErr
	}
	return nil
}
