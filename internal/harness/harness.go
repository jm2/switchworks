// SPDX-License-Identifier: GPL-3.0-or-later

// Package harness constructs adapters by name, so the node and the dev
// commands share one list of supported harnesses.
package harness

import (
	"fmt"
	"io"
	"os/exec"
	"sort"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/adapter/agyad"
	"github.com/jm2/switchyard/internal/adapter/codexad"
	"github.com/jm2/switchyard/internal/jsonrpc"
)

// Options configure every adapter a host may run.
type Options struct {
	CodexBin  string            // default "codex"
	CodexArgs []string          // default ["app-server"]
	CodexEnv  map[string]string // e.g. CODEX_HOME
	AgyBin    string            // default "agy"
	AgyArgs   []string          // extra agy flags
	AgyEnv    map[string]string
	Stderr    io.Writer
	Tap       jsonrpc.Tap // raw protocol capture (dev run --record)
	Logf      func(format string, args ...any)
}

// Names lists every harness this build supports.
func Names() []string { return []string{codexad.Harness, agyad.Harness} }

// New builds the adapter for harness.
func New(name string, o Options) (adapter.Adapter, error) {
	switch name {
	case codexad.Harness, "":
		return codexad.New(codexad.Config{
			Bin: o.CodexBin, Args: o.CodexArgs, Env: o.CodexEnv, Stderr: o.Stderr, Tap: o.Tap, Logf: o.Logf,
		}), nil
	case agyad.Harness:
		return agyad.New(agyad.Config{
			Bin: o.AgyBin, ExtraArgs: o.AgyArgs, Env: o.AgyEnv, Stderr: o.Stderr, Tap: o.Tap, Logf: o.Logf,
		}), nil
	}
	return nil, fmt.Errorf("unknown harness %q (supported: %v)", name, Names())
}

// Available returns the harnesses whose binaries resolve on this host.
func Available(o Options) []string {
	var out []string
	if bin := orDefault(o.CodexBin, "codex"); found(bin) {
		out = append(out, codexad.Harness)
	}
	if bin := orDefault(o.AgyBin, "agy"); found(bin) {
		out = append(out, agyad.Harness)
	}
	sort.Strings(out)
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func found(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
