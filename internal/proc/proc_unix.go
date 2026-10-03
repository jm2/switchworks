//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

type platformState struct{}

func setupCmd(cmd *exec.Cmd) {
	// Own process group, so signals reach the whole tree and a terminal
	// Ctrl-C aimed at swyd does not hit the harness directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (s *platformState) attach(cmd *exec.Cmd) error { return nil }
func (s *platformState) release()                   {}

func (s *platformState) terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func (s *platformState) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
