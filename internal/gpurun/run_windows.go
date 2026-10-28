// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package gpurun

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Capability SIDs given with Plan.Network: internetClient, outbound to the
// internet only. Not privateNetworkClientServer, which would also open the
// LAN and inbound connections. Loopback stays closed to AppContainers either
// way (no loopback exemption), so a run cannot reach the node's local API or
// other services on the host.
var networkCapabilities = []string{"S-1-15-3-1"}

// grantMu serialises the workspace walks.
var grantMu sync.Mutex

type startupInfoEx struct {
	syscall.StartupInfo
	AttributeList uintptr
}

type sidAndAttributes struct {
	Sid        uintptr
	Attributes uint32
}

type securityCapabilities struct {
	AppContainerSid uintptr
	Capabilities    uintptr
	CapabilityCount uint32
	Reserved        uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobBasicLimits struct {
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

type jobExtendedLimits struct {
	Basic                 jobBasicLimits
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Run runs p in an AppContainer on the interactive desktop and returns its
// exit code and output. The node calls it from the owner's interactive
// session (its logon task).
func Run(ctx context.Context, p Plan) (Result, error) {
	sid, err := containerSID(ContainerName(p.Workspace))
	if err != nil {
		return Result{}, err
	}
	defer procFreeSid.Call(sid)
	sidStr, err := sidString(sid)
	if err != nil {
		return Result{}, err
	}
	folder, err := containerFolder(sidStr)
	if err != nil {
		return Result{}, err
	}
	for _, d := range FolderDirs {
		if err := os.MkdirAll(filepath.Join(folder, d), 0o700); err != nil {
			return Result{}, err
		}
	}
	grantMu.Lock()
	skipped, err := grant(p.Workspace, sidBytes(sid), p.ReadOnly, p.Hidden)
	grantMu.Unlock()
	if err != nil {
		return Result{}, err
	}

	job, err := newJob()
	if err != nil {
		return Result{}, err
	}
	defer syscall.CloseHandle(job)

	outR, outW, err := inheritablePipe()
	if err != nil {
		return Result{}, err
	}
	errR, errW, err := inheritablePipe()
	if err != nil {
		syscall.CloseHandle(outR)
		syscall.CloseHandle(outW)
		return Result{}, err
	}
	nul, err := openNul()
	if err != nil {
		for _, h := range []syscall.Handle{outR, outW, errR, errW} {
			syscall.CloseHandle(h)
		}
		return Result{}, err
	}
	stdout, stderr := os.NewFile(uintptr(outR), "stdout"), os.NewFile(uintptr(errR), "stderr")

	pi, err := start(p, sid, folder, job, nul, outW, errW)
	syscall.CloseHandle(nul)
	syscall.CloseHandle(outW)
	syscall.CloseHandle(errW)
	if err != nil {
		stdout.Close()
		stderr.Close()
		return Result{}, err
	}
	defer syscall.CloseHandle(pi.Process)
	began := time.Now()

	res := Result{Skipped: skipped}
	outT, errT := NewTail(MaxOutput), NewTail(MaxOutput)
	var copies sync.WaitGroup
	copies.Add(2)
	// Each copy closes its pipe when it ends (a Close would wait for a read
	// in progress).
	for _, c := range []struct {
		t *Tail
		f *os.File
	}{{outT, stdout}, {errT, stderr}} {
		go func() {
			defer copies.Done()
			_, _ = io.Copy(c.t, c.f)
			c.f.Close()
		}()
	}

	exited := make(chan struct{})
	go func() {
		syscall.WaitForSingleObject(pi.Process, syscall.INFINITE)
		close(exited)
	}()
	timer := time.NewTimer(p.Timeout)
	defer timer.Stop()
	var cause error
	select {
	case <-exited:
	case <-timer.C:
		res.TimedOut = true
	case <-ctx.Done():
		cause = ctx.Err()
	}
	// The run ends with its first process; the job takes the rest with it.
	procTerminateJobObject.Call(uintptr(job), 1)
	<-exited
	res.DurationMS = time.Since(began).Milliseconds()
	var code uint32
	if err := syscall.GetExitCodeProcess(pi.Process, &code); err == nil {
		res.ExitCode = int64(code)
	}
	// Every holder of the pipes' write ends was in the job.
	done := make(chan struct{})
	go func() { copies.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	res.Stdout, res.StdoutDropped = outT.String(), outT.Dropped()
	res.Stderr, res.StderrDropped = errT.String(), errT.Dropped()
	if cause != nil {
		return res, cause
	}
	return res, nil
}

func newJob() (syscall.Handle, error) {
	j, _, err := procCreateJobObjectW.Call(0, 0)
	if j == 0 {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	job := syscall.Handle(j)
	var lim jobExtendedLimits
	lim.Basic.LimitFlags = jobLimitKillOnJobClose | jobLimitDieOnUnhandledException
	if r, _, err := procSetInformationJobObject.Call(j, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&lim)), unsafe.Sizeof(lim)); r == 0 {
		syscall.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject (limits): %w", err)
	}
	ui := uint32(uiLimits)
	if r, _, err := procSetInformationJobObject.Call(j, jobObjectBasicUIRestrictions,
		uintptr(unsafe.Pointer(&ui)), unsafe.Sizeof(ui)); r == 0 {
		syscall.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject (UI limits): %w", err)
	}
	return job, nil
}

// inheritablePipe returns a pipe whose write end only is inheritable.
func inheritablePipe() (r, w syscall.Handle, err error) {
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
	if err := syscall.CreatePipe(&r, &w, &sa, 0); err != nil {
		return 0, 0, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := syscall.SetHandleInformation(r, syscall.HANDLE_FLAG_INHERIT, 0); err != nil {
		syscall.CloseHandle(r)
		syscall.CloseHandle(w)
		return 0, 0, err
	}
	return r, w, nil
}

func openNul() (syscall.Handle, error) {
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
	n, _ := syscall.UTF16PtrFromString("NUL")
	return syscall.CreateFile(n, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, &sa, syscall.OPEN_EXISTING, 0, 0)
}

// start creates the process suspended in the AppContainer, inheriting only
// its three standard handles, puts it in job and lets it run.
func start(p Plan, sid uintptr, folder string, job, stdin, stdout, stderr syscall.Handle) (*syscall.ProcessInformation, error) {
	// Everything the attribute list points to lives outside the Go heap.
	var caps []uintptr
	defer func() {
		for _, c := range caps {
			localFree(c)
		}
	}()
	if p.Network {
		for _, s := range networkCapabilities {
			c, err := stringSID(s)
			if err != nil {
				return nil, err
			}
			caps = append(caps, c)
		}
	}
	nCaps := max(len(caps), 1)
	capArr, err := localAlloc(uintptr(nCaps) * unsafe.Sizeof(sidAndAttributes{}))
	if err != nil {
		return nil, err
	}
	defer localFree(capArr)
	for i, c := range caps {
		*(*sidAndAttributes)(at(capArr, uintptr(i)*unsafe.Sizeof(sidAndAttributes{}))) = sidAndAttributes{Sid: c, Attributes: seGroupEnabled}
	}
	sc, err := localAlloc(unsafe.Sizeof(securityCapabilities{}))
	if err != nil {
		return nil, err
	}
	defer localFree(sc)
	*(*securityCapabilities)(at(sc, 0)) = securityCapabilities{AppContainerSid: sid, CapabilityCount: uint32(len(caps))}
	if len(caps) > 0 {
		(*securityCapabilities)(at(sc, 0)).Capabilities = capArr
	}
	handles, err := localAlloc(3 * unsafe.Sizeof(syscall.Handle(0)))
	if err != nil {
		return nil, err
	}
	defer localFree(handles)
	hs := unsafe.Slice((*syscall.Handle)(at(handles, 0)), 3)
	hs[0], hs[1], hs[2] = stdin, stdout, stderr

	var size uintptr
	procInitializeProcThreadAttributeList.Call(0, 2, 0, uintptr(unsafe.Pointer(&size)))
	list, err := localAlloc(size)
	if err != nil {
		return nil, err
	}
	defer localFree(list)
	if r, _, err := procInitializeProcThreadAttributeList.Call(list, 2, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %w", err)
	}
	defer procDeleteProcThreadAttributeList.Call(list)
	if r, _, err := procUpdateProcThreadAttribute.Call(list, 0, procThreadAttributeSecurityCapabilities,
		sc, unsafe.Sizeof(securityCapabilities{}), 0, 0); r == 0 {
		return nil, fmt.Errorf("UpdateProcThreadAttribute (AppContainer): %w", err)
	}
	if r, _, err := procUpdateProcThreadAttribute.Call(list, 0, procThreadAttributeHandleList,
		handles, 3*unsafe.Sizeof(syscall.Handle(0)), 0, 0); r == 0 {
		return nil, fmt.Errorf("UpdateProcThreadAttribute (handles): %w", err)
	}

	app, err := syscall.UTF16PtrFromString(p.Program)
	if err != nil {
		return nil, err
	}
	cmdline, err := syscall.UTF16FromString(CommandLine(p.Argv))
	if err != nil {
		return nil, err
	}
	cwd, err := syscall.UTF16PtrFromString(p.Cwd)
	if err != nil {
		return nil, err
	}
	env := EnvBlock(Environment(os.Getenv, folder, p.Env))

	si := startupInfoEx{AttributeList: list}
	si.Cb = uint32(unsafe.Sizeof(si))
	if p.Desktop != "" {
		if si.Desktop, err = syscall.UTF16PtrFromString(p.Desktop); err != nil {
			return nil, err
		}
	}
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = stdin, stdout, stderr
	pi := new(syscall.ProcessInformation)
	err = syscall.CreateProcess(app, &cmdline[0], nil, nil, true,
		createSuspended|createNewProcessGroup|createUnicodeEnvironment|extendedStartupInfoPresent|createNoWindow,
		&env[0], cwd, &si.StartupInfo, pi)
	runtime.KeepAlive(env)
	runtime.KeepAlive(cmdline)
	if err != nil {
		return nil, fmt.Errorf("start %s in the AppContainer: %w", p.Program, err)
	}
	defer syscall.CloseHandle(pi.Thread)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(pi.Process)); r == 0 {
		syscall.TerminateProcess(pi.Process, 1)
		syscall.CloseHandle(pi.Process)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if r, _, err := procResumeThread.Call(uintptr(pi.Thread)); uint32(r) == 0xFFFFFFFF {
		procTerminateJobObject.Call(uintptr(job), 1)
		syscall.CloseHandle(pi.Process)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	return pi, nil
}
