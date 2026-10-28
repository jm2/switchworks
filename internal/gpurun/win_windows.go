// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package gpurun

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 calls, through the DLLs directly, as internal/proc does, so the
// module needs no third-party dependencies.

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	userenv  = syscall.NewLazyDLL("userenv.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	procLocalAlloc                        = kernel32.NewProc("LocalAlloc")
	procLocalFree                         = kernel32.NewProc("LocalFree")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateJobObjectW                  = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject           = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject          = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject                = kernel32.NewProc("TerminateJobObject")
	procResumeThread                      = kernel32.NewProc("ResumeThread")
	procGetFinalPathNameByHandleW         = kernel32.NewProc("GetFinalPathNameByHandleW")

	procGetSecurityInfo              = advapi32.NewProc("GetSecurityInfo")
	procSetKernelObjectSecurity      = advapi32.NewProc("SetKernelObjectSecurity")
	procInitializeSecurityDescriptor = advapi32.NewProc("InitializeSecurityDescriptor")
	procSetSecurityDescriptorDacl    = advapi32.NewProc("SetSecurityDescriptorDacl")
	procGetSecurityDescriptorControl = advapi32.NewProc("GetSecurityDescriptorControl")
	procSetSecurityDescriptorControl = advapi32.NewProc("SetSecurityDescriptorControl")
	procConvertStringSidToSidW       = advapi32.NewProc("ConvertStringSidToSidW")
	procFreeSid                      = advapi32.NewProc("FreeSid")
	procGetLengthSid                 = advapi32.NewProc("GetLengthSid")

	procCreateAppContainerProfile                 = userenv.NewProc("CreateAppContainerProfile")
	procDeriveAppContainerSidFromAppContainerName = userenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	procGetAppContainerFolderPath                 = userenv.NewProc("GetAppContainerFolderPath")
	procCoTaskMemFree                             = ole32.NewProc("CoTaskMemFree")
)

const (
	lptr = 0x40 // LMEM_FIXED | LMEM_ZEROINIT

	seFileObject              = 1
	daclSecurityInformation   = 4
	securityDescriptorRev     = 1
	securityDescriptorMinSize = 40 // SECURITY_DESCRIPTOR_MIN_LENGTH on 64-bit
	seDACLAutoInherited       = 0x0400
	seDACLProtected           = 0x1000

	hresultAlreadyExists = 0x800700B7 // HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS)
	seGroupEnabled       = 0x4

	procThreadAttributeHandleList           = 0x20002
	procThreadAttributeSecurityCapabilities = 0x20009

	createSuspended            = 0x00000004
	createNewProcessGroup      = 0x00000200
	createUnicodeEnvironment   = 0x00000400
	extendedStartupInfoPresent = 0x00080000
	createNoWindow             = 0x08000000

	jobObjectBasicUIRestrictions      = 4
	jobObjectExtendedLimitInformation = 9
	jobLimitDieOnUnhandledException   = 0x00000400
	jobLimitKillOnJobClose            = 0x00002000
	// UI limits: no other processes' USER handles (windows), no clipboard,
	// no system parameters, no global atoms, no desktop creation or
	// switching, no logoff or shutdown. Display settings stay allowed, for
	// games that change mode in full screen.
	uiLimitHandles          = 0x0001
	uiLimitReadClipboard    = 0x0002
	uiLimitWriteClipboard   = 0x0004
	uiLimitSystemParameters = 0x0008
	uiLimitGlobalAtoms      = 0x0020
	uiLimitDesktop          = 0x0040
	uiLimitExitWindows      = 0x0080
	uiLimits                = uiLimitHandles | uiLimitReadClipboard | uiLimitWriteClipboard | uiLimitSystemParameters |
		uiLimitGlobalAtoms | uiLimitDesktop | uiLimitExitWindows

	fileFlagBackupSemantics  = 0x02000000
	fileFlagOpenReparsePoint = 0x00200000
	fileAttributeReparse     = 0x400
	readControl              = 0x00020000
	writeDAC                 = 0x00040000
	fileReadAttributes       = 0x80
)

func localAlloc(n uintptr) (uintptr, error) {
	p, _, err := procLocalAlloc.Call(lptr, n)
	if p == 0 {
		return 0, fmt.Errorf("LocalAlloc: %w", err)
	}
	return p, nil
}

func localFree(p uintptr) {
	if p != 0 {
		procLocalFree.Call(p)
	}
}

func hresult(name string, r uintptr) error {
	if uint32(r) == 0 {
		return nil
	}
	return fmt.Errorf("%s: HRESULT 0x%08X", name, uint32(r))
}

// sidBytes copies the SID at p.
func sidBytes(p uintptr) []byte {
	n, _, _ := procGetLengthSid.Call(p)
	return append([]byte(nil), unsafe.Slice((*byte)(at(p, 0)), n)...)
}

// sidString formats the SID at p.
func sidString(p uintptr) (string, error) {
	return (*syscall.SID)(at(p, 0)).String()
}

// containerSID returns the SID of AppContainer name, creating its profile
// (and so its folder) the first time. Free it with FreeSid.
func containerSID(name string) (uintptr, error) {
	n, _ := syscall.UTF16PtrFromString(name)
	disp, _ := syscall.UTF16PtrFromString("Switchworks GPU run")
	var sid uintptr
	r, _, _ := procCreateAppContainerProfile.Call(uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(disp)),
		uintptr(unsafe.Pointer(disp)), 0, 0, uintptr(unsafe.Pointer(&sid)))
	if uint32(r) == hresultAlreadyExists {
		r, _, _ = procDeriveAppContainerSidFromAppContainerName.Call(uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(&sid)))
		if err := hresult("DeriveAppContainerSidFromAppContainerName", r); err != nil {
			return 0, err
		}
		return sid, nil
	}
	if err := hresult("CreateAppContainerProfile", r); err != nil {
		return 0, err
	}
	return sid, nil
}

// containerFolder is the AppContainer's own folder
// (%LOCALAPPDATA%\Packages\<name>\AC), which it may write.
func containerFolder(sid string) (string, error) {
	s, _ := syscall.UTF16PtrFromString(sid)
	var p *uint16
	r, _, _ := procGetAppContainerFolderPath.Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(&p)))
	if err := hresult("GetAppContainerFolderPath", r); err != nil {
		return "", err
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return utf16PtrToString(p), nil
}

// stringSID converts a SID string; free it with localFree.
func stringSID(s string) (uintptr, error) {
	p, _ := syscall.UTF16PtrFromString(s)
	var sid uintptr
	r, _, err := procConvertStringSidToSidW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&sid)))
	if r == 0 {
		return 0, fmt.Errorf("ConvertStringSidToSid(%s): %w", s, err)
	}
	return sid, nil
}

// utf16PtrToString reads the NUL-terminated string at p.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; n++ {
		ptr = unsafe.Add(ptr, 2)
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// at is the address off bytes past p, memory Windows allocated (LocalAlloc,
// a SID or security descriptor it returned), never the Go heap. The
// conversion goes through a pointer to the uintptr so vet's unsafeptr check
// sees it is deliberate.
func at(p, off uintptr) unsafe.Pointer {
	p += off
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}
