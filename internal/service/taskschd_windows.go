// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

// Task Scheduler through its COM interface, in this process. v0.1.0 wrote
// the task to a file in %TEMP% and ran schtasks.exe /Create on it, which
// Microsoft Defender's behavior monitoring took for malware setting up
// persistence (Behavior:Win32/Persistence.A!ml) and quarantined swk.exe.
// Here there is no task file and no child process.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// VARIANT arguments are passed by pointer below, which is how 64-bit
// Windows passes them; 32-bit Windows passes them by value. This fails to
// compile on a 32-bit build.
var _ [unsafe.Sizeof(uintptr(0)) - 8]byte

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	oleaut32             = syscall.NewLazyDLL("oleaut32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procSysAllocString   = oleaut32.NewProc("SysAllocString")
	procSysFreeString    = oleaut32.NewProc("SysFreeString")

	clsidTaskScheduler = syscall.GUID{Data1: 0x0f87369f, Data2: 0xa4e5, Data3: 0x4cfc, Data4: [8]byte{0xbd, 0x3e, 0x73, 0xe6, 0x15, 0x45, 0x72, 0xdd}}
	iidITaskService    = syscall.GUID{Data1: 0x2faba4c7, Data2: 0x4da9, Data3: 0x4013, Data4: [8]byte{0x96, 0x97, 0x20, 0xcc, 0x3f, 0xd4, 0x0f, 0x85}}
)

// Method slots in the interfaces' vtables (taskschd.h). Each starts with
// IUnknown (0-2) and IDispatch (3-6).
const (
	release = 2

	serviceGetFolder = 7
	serviceConnect   = 10

	folderGetTask      = 13
	folderDeleteTask   = 15
	folderRegisterTask = 16

	taskGetState   = 9
	taskGetEnabled = 10
	taskRun        = 12
	taskGetXML     = 20
	taskStop       = 23
)

const (
	taskCreateOrUpdate        = 6 // TASK_CREATE_OR_UPDATE
	taskLogonInteractiveToken = 3 // TASK_LOGON_INTERACTIVE_TOKEN
	taskStateRunning          = 4 // TASK_STATE_RUNNING
)

// comObject is a COM interface pointer: its first word is the vtable.
type comObject struct{ vtbl *[32]uintptr }

// call calls method i with the object as its first argument.
//
//go:uintptrescapes
func (o *comObject) call(i int, args ...uintptr) error {
	hr, _, _ := syscall.SyscallN(o.vtbl[i], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return hresult(hr)
}

func (o *comObject) release() {
	_, _, _ = syscall.SyscallN(o.vtbl[release], uintptr(unsafe.Pointer(o)))
}

// variant is a VARIANT; the zero value is VT_EMPTY, an omitted argument.
type variant struct {
	vt  uint16
	_   [3]uint16
	val [2]uintptr
}

// hresultError is a failed HRESULT. errors.Is matches it against the
// syscall.Errno it carries (0x8007xxxx), such as ERROR_FILE_NOT_FOUND for
// a task that does not exist.
type hresultError uint32

func hresult(hr uintptr) error {
	if int32(hr) >= 0 {
		return nil
	}
	return hresultError(hr)
}

func (e hresultError) Error() string {
	if msg := syscall.Errno(e).Error(); !strings.HasPrefix(msg, "winapi error") {
		return msg
	}
	return fmt.Sprintf("HRESULT 0x%08X", uint32(e))
}

func (e hresultError) Is(target error) bool {
	n, ok := target.(syscall.Errno)
	return ok && uint32(e) == 0x80070000|uint32(n)&0xFFFF
}

// withBSTR passes s to f as a BSTR.
func withBSTR(s string, f func(b uintptr) error) error {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return err
	}
	b, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(p)))
	if b == 0 {
		return errors.New("SysAllocString: out of memory")
	}
	defer procSysFreeString.Call(b)
	return f(b)
}

// takeBSTR returns the text of a BSTR a method returned, and frees it.
func takeBSTR(b *uint16) string {
	if b == nil {
		return ""
	}
	defer procSysFreeString.Call(uintptr(unsafe.Pointer(b)))
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(b), 2*n)) != 0 {
		n++
	}
	return string(utf16.Decode(unsafe.Slice(b, n)))
}

// withTaskFolder runs f with Task Scheduler's root folder. COM is set up
// per thread, so this runs on a new thread that ends with it: nothing is
// left set up on a thread Go may later give to other code, such as the
// tray's window thread.
func withTaskFolder(f func(root *comObject) error) error {
	done := make(chan error, 1)
	go func() {
		// Never unlocked: the thread exits with this goroutine.
		runtime.LockOSThread()
		done <- func() error {
			const coinitMultithreaded = 0
			hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
			if err := hresult(hr); err != nil {
				return fmt.Errorf("Task Scheduler: CoInitializeEx: %w", err)
			}
			defer procCoUninitialize.Call()
			const clsctxInprocServer = 1
			var svc *comObject
			hr, _, _ = procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidTaskScheduler)), 0, clsctxInprocServer,
				uintptr(unsafe.Pointer(&iidITaskService)), uintptr(unsafe.Pointer(&svc)))
			if err := hresult(hr); err != nil {
				return fmt.Errorf("Task Scheduler: %w", err)
			}
			defer svc.release()
			var server, user, domain, password variant
			if err := svc.call(serviceConnect, uintptr(unsafe.Pointer(&server)), uintptr(unsafe.Pointer(&user)),
				uintptr(unsafe.Pointer(&domain)), uintptr(unsafe.Pointer(&password))); err != nil {
				return fmt.Errorf("Task Scheduler: connect: %w", err)
			}
			var root *comObject
			if err := withBSTR(`\`, func(path uintptr) error {
				return svc.call(serviceGetFolder, path, uintptr(unsafe.Pointer(&root)))
			}); err != nil {
				return fmt.Errorf("Task Scheduler: %w", err)
			}
			defer root.release()
			return f(root)
		}()
	}()
	return <-done
}

// withTask runs f with the task called name in the root folder. The error
// matches syscall.ERROR_FILE_NOT_FOUND if there is none.
func withTask(name string, f func(task *comObject) error) error {
	return withTaskFolder(func(root *comObject) error {
		var task *comObject
		if err := withBSTR(name, func(b uintptr) error {
			return root.call(folderGetTask, b, uintptr(unsafe.Pointer(&task)))
		}); err != nil {
			return err
		}
		defer task.release()
		return f(task)
	})
}

// registerTask creates or replaces the task called name in the root
// folder, as defined by the task XML, for the user it names.
func registerTask(name, xmlText string) error {
	return withTaskFolder(func(root *comObject) error {
		return withBSTR(name, func(n uintptr) error {
			return withBSTR(xmlText, func(x uintptr) error {
				var user, password, sddl variant
				var task *comObject
				err := root.call(folderRegisterTask, n, x, taskCreateOrUpdate,
					uintptr(unsafe.Pointer(&user)), uintptr(unsafe.Pointer(&password)), taskLogonInteractiveToken,
					uintptr(unsafe.Pointer(&sddl)), uintptr(unsafe.Pointer(&task)))
				if err != nil {
					return fmt.Errorf("registering the task %q: %w", name, err)
				}
				task.release()
				return nil
			})
		})
	})
}

func deleteTask(name string) error {
	return withTaskFolder(func(root *comObject) error {
		return withBSTR(name, func(b uintptr) error { return root.call(folderDeleteTask, b, 0) })
	})
}

// runTask starts the task now, as Task Scheduler would at logon.
func runTask(name string) error {
	return withTask(name, func(task *comObject) error {
		var params variant
		var running *comObject
		if err := task.call(taskRun, uintptr(unsafe.Pointer(&params)), uintptr(unsafe.Pointer(&running))); err != nil {
			return err
		}
		running.release()
		return nil
	})
}

// stopTask ends every running instance of the task.
func stopTask(name string) error {
	return withTask(name, func(task *comObject) error { return task.call(taskStop, 0) })
}

type taskInfo struct {
	enabled, running bool
	xml              string
}

func queryTask(name string) (taskInfo, error) {
	var in taskInfo
	err := withTask(name, func(task *comObject) error {
		var state int32
		var enabled int16 // VARIANT_BOOL
		var x *uint16
		if err := task.call(taskGetState, uintptr(unsafe.Pointer(&state))); err != nil {
			return err
		}
		if err := task.call(taskGetEnabled, uintptr(unsafe.Pointer(&enabled))); err != nil {
			return err
		}
		if err := task.call(taskGetXML, uintptr(unsafe.Pointer(&x))); err != nil {
			return err
		}
		in = taskInfo{enabled: enabled != 0, running: state == taskStateRunning, xml: takeBSTR(x)}
		return nil
	})
	return in, err
}
