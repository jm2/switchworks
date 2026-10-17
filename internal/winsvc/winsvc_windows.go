// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jm2/switchworks/internal/winsys"
)

var (
	advapi32                          = winsys.DLL("advapi32.dll")
	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
)

const (
	serviceWin32OwnProcess = 0x10

	stateStopped     = 1
	stateStopPending = 3
	stateRunning     = 4

	acceptStop     = 0x1
	acceptShutdown = 0x4

	controlStop     = 1
	controlShutdown = 5

	errorServiceSpecificError     = 1066
	errorFailedServiceControllerC = syscall.Errno(1063)
)

// serviceStatus is SERVICE_STATUS.
type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

// tableEntry is SERVICE_TABLE_ENTRYW.
type tableEntry struct {
	name *uint16
	proc uintptr
}

// The service control manager calls back on threads of its own, with no
// way to pass Go values through, so the one service's state is global.
var svc struct {
	body   func(context.Context) int
	ctx    context.Context
	cancel context.CancelFunc
	code   int

	mu     sync.Mutex
	handle uintptr
}

var (
	// An own-process service's name in the table is ignored; it must not
	// be empty.
	serviceName, _ = syscall.UTF16PtrFromString("swk")
	mainCallback   = syscall.NewCallback(serviceMain)
	ctrlCallback   = syscall.NewCallback(handler)
)

func run(body func(context.Context) int) (int, error) {
	svc.body = body
	svc.ctx, svc.cancel = context.WithCancel(context.Background())
	defer svc.cancel()
	table := [2]tableEntry{{serviceName, mainCallback}, {}}
	// Blocks until the service has stopped.
	if r, _, err := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0]))); r == 0 {
		if errors.Is(err, errorFailedServiceControllerC) {
			return 1, ErrNotService
		}
		return 1, err
	}
	return svc.code, nil
}

func setStatus(state, accepts, exit, specific, waitHint uint32) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.handle == 0 {
		return
	}
	st := serviceStatus{
		ServiceType: serviceWin32OwnProcess, CurrentState: state, ControlsAccepted: accepts,
		Win32ExitCode: exit, ServiceSpecificExitCode: specific, WaitHint: waitHint,
	}
	procSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(&st)))
}

// serviceMain is ServiceMain: it runs the body and reports the service
// stopped when it returns.
func serviceMain(argc, argv uintptr) uintptr {
	h, _, _ := procRegisterServiceCtrlHandlerExW.Call(uintptr(unsafe.Pointer(serviceName)), ctrlCallback, 0)
	if h == 0 {
		svc.code = 1
		return 0
	}
	svc.mu.Lock()
	svc.handle = h
	svc.mu.Unlock()
	setStatus(stateRunning, acceptStop|acceptShutdown, 0, 0, 0)
	code := svc.body(svc.ctx)
	svc.code = code
	var exit, specific uint32
	if code != 0 {
		exit, specific = errorServiceSpecificError, uint32(code)
	}
	setStatus(stateStopped, 0, exit, specific, 0)
	return 0
}

// handler is the HandlerEx: stop and shutdown cancel the body's context.
// Every other control (such as interrogate) needs no more than NO_ERROR.
func handler(ctl, eventType, eventData, context uintptr) uintptr {
	switch uint32(ctl) {
	case controlStop, controlShutdown:
		setStatus(stateStopPending, 0, 0, 0, 30000)
		svc.cancel()
	}
	return 0
}
