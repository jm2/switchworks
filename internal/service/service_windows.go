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
	"unicode/utf16"

	"github.com/jm2/switchyard/internal/runstatus"
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

func schtasksExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "schtasks.exe")
}

func schtasks(args ...string) (string, error) { return run(schtasksExe(), args...) }

// Task names are machine-wide, so they carry the user's name: two people on
// one PC each get their own task.
func unitName(role string) string {
	name := "user"
	if u, err := user.Current(); err == nil {
		name = u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
	}
	return "Switchyard " + role + " (" + name + ")"
}

func query(role string) (state, error) {
	var st state
	// The exit status is all that is used: the text is localized.
	if command(schtasksExe(), "/Query", "/TN", unitName(role)).Run() == nil {
		st.installed, st.enabled = true, true
	}
	// Whether it runs comes from the node's status file (see Query).
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
    <Description>Switchyard ` + role + ` for this user. Written by swyd service install; swyd service uninstall removes it.</Description>
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

// utf16File encodes s as UTF-16LE with a byte-order mark, the encoding
// schtasks /XML expects to match the XML declaration.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// backgroundExe picks swydw.exe, the windowless build of swyd, beside exe:
// a console program started at logon would open a console window.
func backgroundExe(exe string) (string, bool) {
	w := filepath.Join(filepath.Dir(exe), "swydw.exe")
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
		fmt.Fprintf(out, "Note: swydw.exe is not next to %s, so the task runs swyd.exe in a console window.\n", exe)
	}
	logFile := filepath.Join(LogDir(role), role+".log")
	args := syscall.EscapeArg(role) + " run --log-file " + syscall.EscapeArg(logFile)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "swyd-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16File(taskXML(role, u.Uid, bg, args, home))); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	name := unitName(role)
	if _, err := schtasks("/Create", "/TN", name, "/XML", f.Name(), "/F"); err != nil {
		return err
	}
	fmt.Fprintf(out, "Registered the logon task %q (runs %s; log %s)\n", name, bg, logFile)
	if noStart {
		return nil
	}
	if _, err := schtasks("/Run", "/TN", name); err != nil {
		return err
	}
	fmt.Fprintln(out, "Started it.")
	return nil
}

func uninstall(role string, out io.Writer) error {
	st, _ := query(role)
	if !st.installed {
		return nil
	}
	_ = stop(role)
	if _, err := schtasks("/Delete", "/TN", unitName(role), "/F"); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed the logon task %q\n", unitName(role))
	return nil
}

func start(role string) error {
	_, err := schtasks("/Run", "/TN", unitName(role))
	return err
}

// stop asks the process to stop through its stop event, so it can save its
// state and end its harnesses, and ends the task only if that fails.
func stop(role string) error {
	err := runstatus.RequestStop(Dir(role))
	if err == nil && waitStopped(role, 25*time.Second) {
		return nil
	}
	if _, endErr := schtasks("/End", "/TN", unitName(role)); endErr != nil && err == nil {
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
	// Task Scheduler refuses /Run while it still counts the old instance.
	time.Sleep(time.Second)
	return start(role)
}
