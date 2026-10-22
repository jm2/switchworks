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
	"sync"
	"syscall"
	"time"
)

// treeGrace is how long what an exited leader left in its process group
// gets to exit after SIGTERM before it is killed.
const treeGrace = 2 * time.Second

// platformState knows whether the leader may have been reaped. The group
// id is the leader's pid, which is free for reuse once the leader is
// reaped and the group has emptied, so Stop and Kill signal the group
// only before then: afterwards they could reach an unrelated process
// group that took the id.
type platformState struct {
	mu     sync.Mutex
	reaped bool
}

func setupCmd(cmd *exec.Cmd) {
	// Own process group, so signals reach the whole tree and a terminal
	// Ctrl-C aimed at swk does not hit the harness directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (s *platformState) attach(cmd *exec.Cmd) error { return nil }
func (s *platformState) release()                   {}

// wait waits for the leader, ends what it left in its group, and reaps it.
// Where the exit can be seen without reaping (Linux), the group is ended
// first, while the unreaped leader keeps its id from being reused, and
// group signals stop before the reap. Elsewhere the group is ended after
// the reap, which relies on its remaining members keeping the id taken.
func (s *platformState) wait(cmd *exec.Cmd) error {
	if waitExited(cmd.Process.Pid) {
		s.endTree(cmd)
		s.mu.Lock()
		s.reaped = true
		s.mu.Unlock()
		return cmd.Wait()
	}
	err := cmd.Wait()
	s.mu.Lock()
	s.reaped = true
	s.mu.Unlock()
	s.endTree(cmd)
	return err
}

// endTree ends the rest of the process group once the leader has exited:
// SIGTERM, then SIGKILL after treeGrace if anything is still there. The
// group id stays taken while the leader is unreaped or any member is
// alive, so the signals cannot reach an unrelated group.
func (s *platformState) endTree(cmd *exec.Cmd) {
	pgid := -cmd.Process.Pid
	if !groupAlive(cmd.Process.Pid) || syscall.Kill(pgid, syscall.SIGTERM) == syscall.ESRCH {
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

// signalGroup signals the leader's process group unless the leader may
// have been reaped, in which case wait (endTree) has the group in hand.
func (s *platformState) signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// ErrProcessDone: reaped, though wait has not marked it yet.
	if s.reaped || cmd.Process.Signal(syscall.Signal(0)) == os.ErrProcessDone {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func (s *platformState) terminate(cmd *exec.Cmd) { _ = s.signalGroup(cmd, syscall.SIGTERM) }

func (s *platformState) kill(cmd *exec.Cmd) error { return s.signalGroup(cmd, syscall.SIGKILL) }

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
