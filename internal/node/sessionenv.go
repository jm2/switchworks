// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SessionEnv is node.json codex.session_env: variables for the commands
// Codex sessions run. The node applies it to the environment it starts
// codex app-server with, and Codex's default shell_environment_policy
// (inherit = "all") passes that on to every command, after which Codex
// still adds its own PATH entries (its apply_patch shim, the sandbox's
// helpers). A shell_environment_policy.set.PATH would freeze a copy of
// those, so PATH is never rendered that way.
type SessionEnv struct {
	// Set sets variables, replacing the node's value.
	Set map[string]string `json:"set,omitempty"`
	// PathPrepend and PathAppend add directories to the front or the end
	// of PATH (the node's own, or Set's PATH when it has one).
	PathPrepend []string `json:"path_prepend,omitempty"`
	PathAppend  []string `json:"path_append,omitempty"`
}

// empty reports whether e changes nothing.
func (e SessionEnv) empty() bool {
	return len(e.Set) == 0 && len(e.PathPrepend) == 0 && len(e.PathAppend) == 0
}

// sessionEnv renders codex.session_env as overrides of the app-server's
// environment, given the node's environment (os.Environ) and whether
// variable names ignore case (Windows). PATH is computed from the node's
// PATH, which on Linux and macOS services is the login shell's (the
// service's PATH drop-in or plist), with the OS list separator, and under
// the name the node's environment uses for it ("Path" on Windows), so the
// app-server does not get two of them.
func sessionEnv(e SessionEnv, environ []string, foldCase bool, sep string) (map[string]string, error) {
	if e.empty() {
		return nil, nil
	}
	same := func(a, b string) bool {
		if foldCase {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	lookup := func(name string) (key, value string, ok bool) {
		for _, kv := range environ {
			k, v, _ := strings.Cut(kv, "=")
			if k != "" && same(k, name) {
				key, value, ok = k, v, true // the last one wins, as in os.Getenv
			}
		}
		return
	}
	out := map[string]string{}
	pathKey, pathVal, _ := lookup("PATH")
	if pathKey == "" {
		pathKey = "PATH"
	}
	pathSet := len(e.PathPrepend) > 0 || len(e.PathAppend) > 0
	for k, v := range e.Set {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, fmt.Errorf("node.json codex.session_env.set: bad variable name %q", k)
		}
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("node.json codex.session_env.set.%s: the value has a NUL byte", k)
		}
		if same(k, "CODEX_HOME") {
			return nil, errors.New("node.json codex.session_env.set: use codex.home for CODEX_HOME")
		}
		if same(k, "PATH") {
			pathVal, pathSet = v, true
			continue
		}
		// One spelling per variable, the node's own if it has one.
		if ek, _, ok := lookup(k); ok {
			k = ek
		}
		out[k] = v
	}
	if pathSet {
		dirs, err := pathDirs(e, pathVal, sep, same)
		if err != nil {
			return nil, err
		}
		out[pathKey] = dirs
	}
	return out, nil
}

// pathDirs is base with the session_env directories added, each once.
func pathDirs(e SessionEnv, base, sep string, same func(a, b string) bool) (string, error) {
	var list []string
	add := func(d string) {
		if d == "" {
			return
		}
		for _, x := range list {
			if same(x, d) {
				return
			}
		}
		list = append(list, d)
	}
	extra := func(field string, dirs []string) ([]string, error) {
		out := make([]string, 0, len(dirs))
		for _, d := range dirs {
			x, err := expandHome(d)
			if err != nil {
				return nil, fmt.Errorf("node.json codex.session_env.%s: %w", field, err)
			}
			// A relative entry would make lookups depend on each command's
			// working directory, which an agent controls.
			if !filepath.IsAbs(x) || strings.ContainsRune(x, 0) || strings.Contains(x, sep) {
				return nil, fmt.Errorf("node.json codex.session_env.%s: %q is not an absolute directory", field, d)
			}
			out = append(out, filepath.Clean(x))
		}
		return out, nil
	}
	pre, err := extra("path_prepend", e.PathPrepend)
	if err != nil {
		return "", err
	}
	post, err := extra("path_append", e.PathAppend)
	if err != nil {
		return "", err
	}
	for _, d := range pre {
		add(d)
	}
	for _, d := range strings.Split(base, sep) {
		add(d)
	}
	for _, d := range post {
		add(d)
	}
	return strings.Join(list, sep), nil
}

// codexEnv is the environment overrides the app-server starts with:
// codex.home's CODEX_HOME and codex.session_env.
func codexEnv(cfg Config) (map[string]string, error) {
	env, err := sessionEnv(cfg.Codex.SessionEnv, os.Environ(), runtime.GOOS == "windows", string(os.PathListSeparator))
	if err != nil {
		return nil, err
	}
	if cfg.Codex.Home != "" {
		if env == nil {
			env = map[string]string{}
		}
		env["CODEX_HOME"] = cfg.Codex.Home
	}
	return env, nil
}
