// SPDX-License-Identifier: GPL-3.0-or-later

// Package harness constructs adapters by name, so the node and the dev
// commands share one list of supported harnesses.
package harness

import (
	"fmt"
	"io"
	"os/exec"
	"sort"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/adapter/acpad"
	"github.com/jm2/switchworks/internal/adapter/agyad"
	"github.com/jm2/switchworks/internal/adapter/codexad"
	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/model"
)

// ACPAgent configures one ACP agent harness (node.json "acp" entries).
type ACPAgent struct {
	Bin  string            `json:"bin,omitempty"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// Modes maps approval policies (never, on-request, untrusted) to the
	// agent's session mode ids.
	Modes map[string]string `json:"modes,omitempty"`
	// Disabled hides a built-in agent (goose) even when it is installed.
	Disabled bool `json:"disabled,omitempty"`
}

// Options configure every adapter a host may run.
type Options struct {
	CodexBin  string            // default "codex"
	CodexArgs []string          // default ["app-server"]
	CodexEnv  map[string]string // e.g. CODEX_HOME
	AgyBin    string            // default "agy"
	AgyArgs   []string          // extra agy flags
	AgyEnv    map[string]string
	Agy       agyad.Options // agy behavior (stream mode, fleet tools)

	// ACP lists ACP agents by harness name. "goose" is built in (goose acp)
	// and its entry here, if any, overrides the defaults.
	ACP map[string]ACPAgent

	Stderr io.Writer
	Tap    jsonrpc.Tap // raw protocol capture (dev run --record)
	Logf   func(format string, args ...any)
}

// Names an ACP entry may not take: the other harnesses, and "" (codex).
var reserved = map[string]bool{codexad.Harness: true, agyad.Harness: true, "": true}

// acpConfigs returns the ACP agents, the built-in goose included unless
// disabled.
func acpConfigs(o Options) map[string]acpad.Config {
	out := map[string]acpad.Config{}
	g := acpad.GooseConfig()
	if a, ok := o.ACP[acpad.Goose]; ok {
		if a.Disabled {
			g.Bin = ""
		}
		if a.Bin != "" {
			g.Bin = a.Bin
		}
		if len(a.Args) > 0 {
			g.Args = a.Args
		}
		g.Env, g.Modes = a.Env, a.Modes
	}
	if g.Bin != "" {
		out[acpad.Goose] = g
	}
	for name, a := range o.ACP {
		if name == acpad.Goose || reserved[name] || a.Disabled || a.Bin == "" || model.ValidateName("harness", name) != nil {
			continue
		}
		out[name] = acpad.Config{Harness: name, Bin: a.Bin, Args: a.Args, Env: a.Env, Modes: a.Modes}
	}
	return out
}

// Names lists every harness this configuration supports.
func Names(o Options) []string {
	names := []string{codexad.Harness, agyad.Harness}
	var acpNames []string
	for n := range acpConfigs(o) {
		acpNames = append(acpNames, n)
	}
	sort.Strings(acpNames)
	return append(names, acpNames...)
}

// New builds the adapter for harness.
func New(name string, o Options) (adapter.Adapter, error) {
	switch name {
	case codexad.Harness, "":
		return codexad.New(codexad.Config{
			Bin: o.CodexBin, Args: o.CodexArgs, Env: o.CodexEnv, Stderr: o.Stderr, Tap: o.Tap, Logf: o.Logf,
		}), nil
	case agyad.Harness:
		return agyad.New(agyad.Config{
			Bin: o.AgyBin, ExtraArgs: o.AgyArgs, Env: o.AgyEnv, Options: o.Agy, Stderr: o.Stderr, Tap: o.Tap, Logf: o.Logf,
		}), nil
	}
	if c, ok := acpConfigs(o)[name]; ok {
		c.Stderr, c.Tap, c.Logf = o.Stderr, o.Tap, o.Logf
		return acpad.New(c), nil
	}
	return nil, fmt.Errorf("unknown harness %q (supported: %v)", name, Names(o))
}

// EnforcesSandbox reports whether a harness applies the sandbox setting a
// session is spawned with (Codex's sandbox modes, agy's --sandbox). ACP
// agents do not: they act with the user's full access, subject only to
// their approval mode.
func EnforcesSandbox(name string) bool {
	return name == codexad.Harness || name == agyad.Harness || name == ""
}

// TakesImages reports whether a harness can be given image attachments:
// Codex (localImage inputs) and ACP agents (image content blocks, sent only
// when the agent advertises promptCapabilities.image; each session reports
// that in Caps.Images). agy's headless input documents text only.
func TakesImages(name string) bool {
	return name != agyad.Harness
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
	for name, c := range acpConfigs(o) {
		if found(c.Bin) {
			out = append(out, name)
		}
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
