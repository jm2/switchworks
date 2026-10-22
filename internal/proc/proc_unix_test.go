// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package proc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Output written just before a fast exit is not lost, and a descendant that
// escapes the process group and keeps stdout open does not keep the reader
// blocked for long after the process exits.
func TestStdoutAfterExit(t *testing.T) {
	for i := 0; i < 20; i++ {
		p, err := Start("sh", []string{"-c", "echo last-line"}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p.Stdout)
		<-p.Done()
		if !strings.Contains(string(b), "last-line") {
			t.Fatalf("run %d: lost output %q", i, b)
		}
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	p, err := Start("sh", []string{"-c", "setsid sleep 30 & echo hi; exit 0"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eof := make(chan string)
	go func() { b, _ := io.ReadAll(p.Stdout); eof <- string(b) }()
	<-p.Done()
	select {
	case out := <-eof:
		if !strings.Contains(out, "hi") {
			t.Fatalf("output %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stdout reader still blocked 10s after the process exited")
	}
}

// A leader that exits on stdin EOF does not leave its children running.
func TestStopEndsChildrenOfExitedLeader(t *testing.T) {
	p, err := Start("sh", []string{"-c", "sleep 300 & echo $!; read x; exit 0"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(p.Stdout, &pid); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Stop(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("Stop took %v", d)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("child %d still running after Stop (kill 0: %v)", pid, err)
	}
}

// A group whose only member has exited but is not yet reaped (a zombie)
// counts as gone on Linux, so endTree does not wait out its grace for it.
func TestGroupAliveIgnoresZombies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombies are told apart through /proc")
	}
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil && strings.Contains(string(b), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no zombie: %s %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) != nil {
		t.Skip("kill(-pgid, 0) already ignores zombies here")
	}
	if groupAlive(pid) {
		t.Fatal("a zombie-only group counted as alive")
	}
}

// Once the leader may have been reaped, Stop and Kill leave the group id
// alone on Linux: it can belong to an unrelated group by then. Elsewhere
// the group is signalled while it has members (see signalGroup).
func TestKillSparesGroupAfterReap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux ends the group before the reap")
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := &platformState{reaped: true}
	if err := reaped.kill(cmd); err != nil {
		t.Fatal(err)
	}
	reaped.terminate(cmd)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("group signalled after the reap: %v", err)
	}
	if err := (&platformState{}).kill(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("sleep was not killed")
	}

	p, err := Start("true", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	p.plat.mu.Lock()
	ok := p.plat.reaped
	p.plat.mu.Unlock()
	if !ok {
		t.Fatal("not marked reaped after exit")
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
}

// On Linux the leader's exit is seen without reaping it, so the group is
// ended while the leader's pid, the group id, is still taken.
func TestWaitExitedLeavesZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("waitid with WNOWAIT is Linux only here")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitExited(cmd.Process.Pid) {
		_ = cmd.Wait()
		t.Fatal("waitExited failed")
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), ") Z ") {
		t.Fatalf("not an unreaped zombie: %s %v", b, err)
	}
}
