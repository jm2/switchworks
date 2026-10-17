// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package proc

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// treeGrace is how long what an exited leader left in its process group
// gets to exit after SIGTERM before it is killed.
const treeGrace = 2 * time.Second

type platformState struct{}

func setupCmd(cmd *exec.Cmd) {
	// Own process group, so signals reach the whole tree and a terminal
	// Ctrl-C aimed at swk does not hit the harness directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (s *platformState) attach(cmd *exec.Cmd) error { return nil }
func (s *platformState) release()                   {}

// endTree ends the rest of the process group once the leader has exited:
// SIGTERM, then SIGKILL after treeGrace if anything is still there. The
// group id stays taken while any member is alive, so the signals cannot
// reach an unrelated group.
func (s *platformState) endTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	if syscall.Kill(pgid, syscall.SIGTERM) == syscall.ESRCH {
		return
	}
	for deadline := time.Now().Add(treeGrace); time.Now().Before(deadline); {
		time.Sleep(25 * time.Millisecond)
		if !groupAlive(cmd.Process.Pid) {
			return
		}
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

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

// groupAlive reports whether process group pgid still has a member that is
// not a zombie. kill(-pgid, 0) also counts zombies waiting to be reaped,
// which can no longer do anything; on Linux /proc tells them apart.
func groupAlive(pgid int) bool {
	if syscall.Kill(-pgid, 0) == syscall.ESRCH {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may hold spaces and ')'.
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) < 3 {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err == nil && g == pgid && f[0] != "Z" && f[0] != "X" {
			return true
		}
	}
	return false
}
