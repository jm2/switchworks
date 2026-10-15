// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package tray

import (
	"errors"
	"github.com/jm2/switchyard/internal/winsys"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32           = winsys.DLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")

	advapi32            = winsys.DLL("advapi32.dll")
	procRegCreateKeyExW = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueExW  = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW = advapi32.NewProc("RegDeleteValueW")
)

const (
	swShowNormal                  = 1
	runKey                        = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue                      = "Switchyard"
	errFileNotFound syscall.Errno = 2
)

func init() {
	openURL = func(u string) error {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return errors.New("not a web address: " + u)
		}
		return shellOpen(u)
	}
	openPath = shellOpen
}

func shellOpen(target string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(t)), 0, 0, swShowNormal)
	if r <= 32 {
		return errors.New("could not open " + target)
	}
	return nil
}

const autostartSupported = true

func openRunKey(access uint32) (syscall.Handle, error) {
	var h syscall.Handle
	k, _ := syscall.UTF16PtrFromString(runKey)
	r, _, _ := procRegCreateKeyExW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(k)), 0, 0, 0,
		uintptr(access), 0, uintptr(unsafe.Pointer(&h)), 0)
	if r != 0 {
		return 0, syscall.Errno(r)
	}
	return h, nil
}

func autostartEnabled() bool {
	h, err := openRunKey(syscall.KEY_QUERY_VALUE)
	if err != nil {
		return false
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	var typ, n uint32
	return syscall.RegQueryValueEx(h, name, nil, &typ, nil, &n) == nil && n > 0
}

func setAutostart(on bool) error {
	h, err := openRunKey(syscall.KEY_SET_VALUE)
	if err != nil {
		return err
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	if !on {
		r, _, _ := procRegDeleteValueW.Call(uintptr(h), uintptr(unsafe.Pointer(name)))
		if r != 0 && syscall.Errno(r) != errFileNotFound {
			return syscall.Errno(r)
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe = filepath.Clean(exe)
	data, err := syscall.UTF16FromString(`"` + exe + `"`)
	if err != nil {
		return err
	}
	r, _, _ := procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, syscall.REG_SZ,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}
