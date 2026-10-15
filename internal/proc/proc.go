// SPDX-License-Identifier: GPL-3.0-or-later

// Package proc starts child processes that can be torn down as a whole tree.
//
// On Linux and macOS the child gets its own process group and Kill signals the
// group. On Windows the child is assigned to a job object configured to kill
// every process in the job when the job handle is closed, so a node that dies
// takes its harness processes with it.
package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Options configure a child process.
type Options struct {
	Dir    string
	Env    []string // full environment; nil means inherit
	Stderr io.Writer
	// Stdin/Stdout pipes are always created.
}

// Process is a running child with stdin/stdout pipes.
type Process struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	plat platformState

	done    chan struct{}
	waitErr error
	once    sync.Once
}

// Start launches name with args.
func Start(name string, args []string, opts Options) (*Process, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	setupCmd(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Not cmd.StdoutPipe: Wait closes that pipe as soon as the process
	// exits, which can drop the last lines a short-lived process wrote
	// before the reader gets to them. With our own pipe the reader sees EOF
	// only after everything written has been read.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = stdoutW
	if err := cmd.Start(); err != nil {
		stdout.Close()
		stdoutW.Close()
		return nil, err
	}
	stdoutW.Close() // the child has its copy
	p := &Process{cmd: cmd, Stdin: stdin, Stdout: &selfClosing{f: stdout}, done: make(chan struct{})}
	if err := p.plat.attach(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	go func() {
		p.waitErr = cmd.Wait()
		p.plat.release()
		close(p.done)
	}()
	return p, nil
}

// selfClosing closes the read end of the stdout pipe once reading it fails
// (normally at EOF), since nothing else will.
type selfClosing struct {
	f    *os.File
	once sync.Once
}

func (r *selfClosing) Read(b []byte) (int, error) {
	n, err := r.f.Read(b)
	if err != nil {
		r.Close()
	}
	return n, err
}

func (r *selfClosing) Close() error {
	var err error
	r.once.Do(func() { err = r.f.Close() })
	return err
}

// Pid returns the child's process id.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the exit error once Done is closed.
func (p *Process) Err() error {
	select {
	case <-p.done:
		return p.waitErr
	default:
		return nil
	}
}

// Stop asks the process tree to exit, waiting up to grace before killing it.
// Closing stdin first lets well-behaved stdio servers exit on EOF.
func (p *Process) Stop(grace time.Duration) error {
	var err error
	p.once.Do(func() {
		_ = p.Stdin.Close()
		select {
		case <-p.done:
			return
		case <-time.After(grace / 2):
		}
		p.plat.terminate(p.cmd)
		select {
		case <-p.done:
			return
		case <-time.After(grace / 2):
		}
		err = p.plat.kill(p.cmd)
		<-p.done
	})
	return err
}

// Kill terminates the process tree immediately.
func (p *Process) Kill() error {
	err := p.plat.kill(p.cmd)
	<-p.done
	return err
}

// LookPath resolves a binary like exec.LookPath, returning a clearer error.
func LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New(name + " not found on PATH")
		}
		return "", err
	}
	return path, nil
}

// Environ returns os.Environ() with overrides applied (KEY=VALUE).
func Environ(overrides map[string]string) []string {
	env := os.Environ()
	if len(overrides) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		k := kv
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				k = kv[:i]
				break
			}
		}
		if _, ok := overrides[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// WaitContext waits for exit or ctx cancellation.
func (p *Process) WaitContext(ctx context.Context) error {
	select {
	case <-p.done:
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
