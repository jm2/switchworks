// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jm2/switchworks/internal/runstatus"
)

const managerName = "task-scheduler"

const createNoWindow = 0x08000000

// command runs a helper without flashing a console window, which matters
// when the tray app (a GUI program) calls it.
func command(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return c
}

// Task names are machine-wide, so they carry the user's name and SID: two
// people on one PC, even a local and a domain account with the same short
// name, each get their own task.
func unitName(role string) string {
	name, sid := "user", ""
	if u, err := user.Current(); err == nil {
		name, sid = u.Username, u.Uid
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
	}
	if sid == "" {
		return "Switchworks " + role + " (" + name + ")"
	}
	return "Switchworks " + role + " (" + name + ", " + sid + ")"
}

func query(role string) (state, error) {
	var st state
	t, err := queryTask(unitName(role))
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.installed, st.enabled, st.active = true, t.enabled, t.running
	return st, nil
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// taskXML is a logon task for one user: it runs in their own session with
// least privilege (never as SYSTEM or elevated), has no time limit, and is
// restarted by Task Scheduler if it fails.
func taskXML(role, sid, exe, args, workDir string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Switchworks ` + role + ` for this user. Written by swk service install; swk service uninstall removes it.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlText(sid) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlText(sid) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlText(exe) + `</Command>
      <Arguments>` + xmlText(args) + `</Arguments>
      <WorkingDirectory>` + xmlText(workDir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

// backgroundExe picks swkw.exe, the windowless build of swk, beside exe:
// a console program started at logon would open a console window.
func backgroundExe(exe string) (string, bool) {
	w := filepath.Join(filepath.Dir(exe), "swkw.exe")
	if fi, err := os.Stat(w); err == nil && !fi.IsDir() {
		return w, true
	}
	return exe, false
}

func install(role, exe string, noStart bool, out io.Writer) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	bg, ok := backgroundExe(exe)
	if !ok {
		fmt.Fprintf(out, "Note: swkw.exe is not next to %s, so the task runs swk.exe in a console window.\n", exe)
	}
	logFile := filepath.Join(LogDir(role), role+".log")
	args := syscall.EscapeArg(role) + " run --log-file " + syscall.EscapeArg(logFile)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	name := unitName(role)
	if err := registerTask(name, taskXML(role, u.Uid, bg, args, home)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Registered the logon task %q (runs %s; log %s)\n", name, bg, logFile)
	removeLegacy(role, out)
	if noStart {
		return nil
	}
	if err := runTask(name); err != nil {
		return err
	}
	if !waitRunning(role, startWait) {
		fmt.Fprintf(out, "Started it, but it has not reported running after %s; see %s\n", startWait, logFile)
		return nil
	}
	fmt.Fprintln(out, "Started it.")
	return nil
}

func uninstall(role string, out io.Writer) error {
	removeLegacy(role, out)
	st, _ := query(role)
	if !st.installed {
		return nil
	}
	_ = stop(role)
	if err := deleteTask(unitName(role)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed the logon task %q\n", unitName(role))
	return nil
}

// legacyUnitName is the task name before it carried the SID.
func legacyUnitName(role string) string {
	name := "user"
	if u, err := user.Current(); err == nil {
		name = u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
	}
	return "Switchworks " + role + " (" + name + ")"
}

// removeLegacy deletes a task under the old name, if it is this user's:
// another account with the same short name may own a task by that name.
func removeLegacy(role string, out io.Writer) {
	name := legacyUnitName(role)
	u, err := user.Current()
	if err != nil || name == unitName(role) {
		return
	}
	t, err := queryTask(name)
	if err != nil || !strings.Contains(t.xml, "<UserId>"+u.Uid+"</UserId>") {
		return
	}
	_ = stopTask(name)
	if err := deleteTask(name); err == nil {
		fmt.Fprintf(out, "Removed the old logon task %q\n", name)
	}
}

// startWait is how long start waits for the process to report running.
const startWait = 30 * time.Second

// start runs the task and waits for the process to write its status file,
// so that a status check right after start or restart sees it running.
func start(role string) error {
	if err := runTask(unitName(role)); err != nil {
		return err
	}
	if !waitRunning(role, startWait) {
		return fmt.Errorf("%s: started, but it has not reported running after %s; see %s",
			role, startWait, filepath.Join(LogDir(role), role+".log"))
	}
	return nil
}

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
)

// waitExit waits for the process in role's status file to exit. The status
// file goes away when shutdown starts; the process saves its state and ends
// its harnesses after that, and Task Scheduler will not start the task again
// until it has exited.
func waitExit(role string, pid int, d time.Duration) bool {
	if pid <= 0 {
		return waitStopped(role, d)
	}
	const synchronize = 0x00100000
	h, _, _ := procOpenProcess.Call(synchronize, 0, uintptr(pid))
	if h == 0 {
		return true // already gone
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	r, _, _ := procWaitForSingleObject.Call(h, uintptr(d.Milliseconds()))
	return r == 0 // WAIT_OBJECT_0
}

// stop asks the process to stop through its stop event, so it can save its
// state and end its harnesses, and ends the task only if that fails.
func stop(role string) error {
	pid := 0
	if s, err := runstatus.Read(Dir(role)); err == nil && s.Fresh(time.Now()) {
		pid = s.PID
	}
	err := runstatus.RequestStop(Dir(role))
	if err == nil && waitExit(role, pid, 30*time.Second) {
		return nil
	}
	if endErr := stopTask(unitName(role)); endErr != nil && err == nil {
		return endErr
	}
	if err != nil && !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return nil
}

func restart(role string) error {
	if err := stop(role); err != nil {
		return err
	}
	// Task Scheduler ignores a run while it still counts the old instance
	// as running (MultipleInstancesPolicy IgnoreNew).
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if t, err := queryTask(unitName(role)); err != nil || !t.running {
			break
		}
	}
	return start(role)
}
