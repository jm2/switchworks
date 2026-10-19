// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package proc

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows job objects, through kernel32 directly so the module needs no
// third-party dependencies.

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
	createNewProcessGroup             = 0x00000200
	createNoWindow                    = 0x08000000
)

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformationT struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// platformState holds the job. mu orders release, which runs as the
// process exits, against kill from Stop or Kill, so kill never uses a
// handle that was closed (and possibly reused for another session's job).
type platformState struct {
	mu       sync.Mutex
	job      syscall.Handle
	released bool
}

// setupCmd gives the child a console with no window. The node runs as
// swkw.exe, which has none, so a console program would otherwise get a new
// one: with Windows Terminal as the default terminal it opens as a tab in the
// user's window, and closing that tab ends the harness. The child's own
// console programs share the hidden console.
func setupCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup | createNoWindow}
}

// attach puts the child in a kill-on-close job. There is a short window
// between process creation and assignment in which a grandchild could escape;
// harnesses do not fork that fast in practice, and closing that window would
// need CREATE_SUSPENDED plus thread handles that os/exec does not expose.
func (s *platformState) attach(cmd *exec.Cmd) error {
	job, _, err := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	info := jobObjectExtendedLimitInformationT{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInformationJobObject.Call(job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	ph, _, err := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(cmd.Process.Pid))
	if ph == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(ph))
	r, _, err = procAssignProcessToJobObject.Call(job, ph)
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	s.mu.Lock()
	s.job = syscall.Handle(job)
	s.mu.Unlock()
	return nil
}

// release closes the job once the leader has exited; the job kills
// whatever is left in it as the handle closes.
func (s *platformState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
	if s.job != 0 {
		syscall.CloseHandle(s.job)
		s.job = 0
	}
}

// endTree has nothing to do: release, right after it, closes the job.
func (s *platformState) endTree(cmd *exec.Cmd) {}

// terminate has no gentle equivalent for console-less children; stdin EOF
// (sent by Stop) is the polite signal, so this goes straight to the job.
func (s *platformState) terminate(cmd *exec.Cmd) {}

func (s *platformState) kill(cmd *exec.Cmd) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil // exited, and the job with it
	}
	if s.job != 0 {
		r, _, err := procTerminateJobObject.Call(uintptr(s.job), 1)
		if r == 0 {
			return fmt.Errorf("TerminateJobObject: %w", err)
		}
		return nil
	}
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
