// SPDX-License-Identifier: GPL-3.0-or-later

package agyad

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/proc"
)

// workspaceMCP is agy's per-workspace MCP config, relative to the cwd.
const workspaceMCP = ".agents/mcp_config.json"

// setupFleetTools makes the fleet tools available to agy sessions in
// spec.Cwd and returns warnings for anything it could not do. It never
// removes or rewrites other entries, and never writes credentials to disk.
func setupFleetTools(spec adapter.StartSpec, cfg Config) []string {
	m := spec.MCP
	var warns []string
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "."
	}
	cmd := m.Command
	if abs, err := proc.LookPath(cmd); err == nil {
		cmd = abs
	}
	args := m.Args
	if args == nil {
		args = []string{}
	}
	if err := insideWorkspace(cwd, workspaceMCP); err != nil {
		return append(warns, "fleet tools unavailable: "+err.Error())
	}
	created, err := mergeMCPConfig(cwd, workspaceMCP, m.Name, map[string]any{"command": cmd, "args": args})
	if err != nil {
		return append(warns, "fleet tools unavailable: "+err.Error())
	}
	if created {
		if err := gitExclude(cwd, workspaceMCP); err != nil {
			warns = append(warns, "could not add "+workspaceMCP+" to .git/info/exclude: "+err.Error())
		}
	}
	if spec.Sandbox != "danger-full-access" {
		path := cfg.Options.SettingsPath
		if path == "" {
			home := cfg.Env["HOME"]
			if home == "" {
				home, _ = os.UserHomeDir()
			}
			path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
		}
		rule := fmt.Sprintf("mcp(%s/*)", m.Name)
		if err := addAllowRule(path, rule); err != nil {
			warns = append(warns, fmt.Sprintf("could not add %s to %s (agy will soft-deny the fleet tools): %v", rule, path, err))
		}
	}
	return warns
}

// insideWorkspace refuses to write rel under cwd when the workspace (which
// the agent controls) has made it a symlink, or a path through one, that
// leads somewhere else.
func insideWorkspace(cwd, rel string) error {
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	p := cwd
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil // created fresh, inside cwd
		}
		if err != nil {
			return err
		}
		// ModeIrregular covers Windows reparse points other than symlinks.
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%s is a symlink or reparse point; not writing through it", p)
		}
	}
	real, err := filepath.EvalSymlinks(filepath.Join(cwd, rel))
	if err != nil {
		return err
	}
	if r, err := filepath.Rel(root, real); err != nil || strings.HasPrefix(r, "..") {
		return fmt.Errorf("%s is outside %s", real, root)
	}
	return nil
}

// mergeMCPConfig sets mcpServers[name] in the agy MCP config file rel
// under cwd, keeping everything else. It reports whether it created the
// file. All access goes through an os.Root on cwd, so a symlink the agent
// swaps in after insideWorkspace looked cannot lead the write outside.
func mergeMCPConfig(cwd, rel, name string, server map[string]any) (bool, error) {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return false, err
	}
	defer root.Close()
	path := filepath.Join(cwd, rel)
	doc := map[string]any{}
	b, err := root.ReadFile(rel)
	created := os.IsNotExist(err)
	switch {
	case created:
	case err != nil:
		return false, err
	case len(bytes.TrimSpace(b)) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; not touching it: %v", path, err)
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		if _, exists := doc["mcpServers"]; exists {
			return false, fmt.Errorf("%s: mcpServers is not an object; not touching it", path)
		}
		servers = map[string]any{}
	}
	if old, ok := servers[name].(map[string]any); ok {
		same := old["command"] == server["command"] && reflect.DeepEqual(asStrings(old["args"]), server["args"])
		if same {
			return false, nil
		}
		// Keep fields the user added (disabledTools, cwd, ...).
		for k, v := range old {
			if _, set := server[k]; !set && k != "env" {
				server[k] = v
			}
		}
	}
	servers[name] = server
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return false, err
	}
	return created, writeAtomicIn(root, rel, append(out, '\n'))
}

func asStrings(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// addAllowRule adds rule to permissions.allow in agy's settings.json.
func addAllowRule(path, rule string) error {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return err
	case len(bytes.TrimSpace(b)) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("not valid JSON; not touching it: %v", err)
		}
	}
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		if _, exists := doc["permissions"]; exists {
			return fmt.Errorf("permissions is not an object; not touching it")
		}
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	for _, r := range allow {
		if r == rule || r == "mcp(*)" {
			return nil
		}
	}
	perms["allow"] = append(allow, rule)
	doc["permissions"] = perms
	out, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, append(out, '\n'))
}

// gitExclude lists rel in the enclosing repository's .git/info/exclude so a
// file Switchyard created does not show up in git status.
func gitExclude(cwd, rel string) error {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	for {
		gitDir := filepath.Join(dir, ".git")
		fi, err := os.Stat(gitDir)
		if err == nil {
			if !fi.IsDir() {
				// A worktree or submodule: .git is a "gitdir: <path>" file.
				b, err := os.ReadFile(gitDir)
				if err != nil {
					return err
				}
				p := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:"))
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				gitDir = p
			}
			// The workspace controls .git, so only write into what is
			// really a git directory; a worktree's excludes live in its
			// common directory.
			if !isGitDir(gitDir) {
				return fmt.Errorf("%s is not a git directory", gitDir)
			}
			if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
				c := strings.TrimSpace(string(b))
				if !filepath.IsAbs(c) {
					c = filepath.Join(gitDir, c)
				}
				if isGitDir(c) {
					gitDir = c
				}
			}
			r, err := filepath.Rel(dir, filepath.Join(cwd, rel))
			if err != nil {
				return err
			}
			r = "/" + filepath.ToSlash(r)
			ex := filepath.Join(gitDir, "info", "exclude")
			cur, _ := os.ReadFile(ex)
			for _, l := range strings.Split(string(cur), "\n") {
				if strings.TrimSpace(l) == r {
					return nil
				}
			}
			if err := os.Mkdir(filepath.Dir(ex), 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			if fi, err := os.Lstat(filepath.Dir(ex)); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", filepath.Dir(ex))
			}
			if fi, err := os.Lstat(ex); err == nil && !fi.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", ex)
			}
			f, err := os.OpenFile(ex, os.O_APPEND|os.O_CREATE|os.O_WRONLY|oNoFollow, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			prefix := ""
			if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
				prefix = "\n"
			}
			_, err = fmt.Fprintf(f, "%s%s\n", prefix, r)
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // not in a repository
		}
		dir = parent
	}
}

// isGitDir reports whether dir looks like a git directory: a HEAD file and
// an objects directory (or, for a worktree, a commondir file).
func isGitDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "objects")); err == nil && fi.IsDir() {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, "commondir"))
	return err == nil
}

func writeAtomic(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".swyd-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeAtomicIn is writeAtomic inside root.
func writeAtomicIn(root *os.Root, rel string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := root.Lstat(rel); err == nil && fi.Mode().IsRegular() {
		mode = fi.Mode().Perm()
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(rel), ".swyd-"+hex.EncodeToString(rnd[:]))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Chmod(tmp, mode); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, rel)
}
