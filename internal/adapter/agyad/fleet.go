// SPDX-License-Identifier: GPL-3.0-or-later

package agyad

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/proc"
)

// workspaceMCP is agy's per-workspace MCP config, relative to the cwd.
const workspaceMCP = ".agents/mcp_config.json"

// fleetSetup records what setupFleetTools added to a workspace, so it can
// be taken out again once no session there uses it.
type fleetSetup struct {
	cwd, name   string
	fileCreated bool           // the config file did not exist before
	installed   map[string]any // the entry it wrote
	previous    map[string]any // the entry it replaced, restored on cleanup
	exclude     excludeFile    // .git/info/exclude, if a line was added to it
	line        string         // the line added
}

// setupFleetTools makes the fleet tools available to agy sessions in
// spec.Cwd and returns warnings for anything it could not do, plus a record
// of the workspace entry when it added or replaced it (nil when the entry
// was already there as wanted).
// It never removes or rewrites other entries, and never writes credentials
// to disk.
func setupFleetTools(spec adapter.StartSpec, cfg Config) (*fleetSetup, []string) {
	m := spec.MCP
	var warns []string
	var made *fleetSetup
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
		return nil, append(warns, "fleet tools unavailable: "+err.Error())
	}
	server := map[string]any{"command": cmd, "args": args}
	created, added, previous, err := mergeMCPConfig(cwd, workspaceMCP, m.Name, server)
	if err != nil {
		return nil, append(warns, "fleet tools unavailable: "+err.Error())
	}
	if added || previous != nil {
		// server is what was written, the user's extra fields included.
		made = &fleetSetup{cwd: cwd, name: m.Name, fileCreated: created, installed: server, previous: previous}
	}
	if created {
		if made.exclude, made.line, err = gitExclude(cwd, workspaceMCP); err != nil {
			warns = append(warns, "could not add "+workspaceMCP+" to .git/info/exclude: "+err.Error())
		}
	}
	if !skipsPermissions(spec) {
		path := cfg.Options.SettingsPath
		if path == "" {
			home := cfg.Env["HOME"]
			if home == "" {
				home, _ = os.UserHomeDir()
			}
			path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
		}
		rule := fmt.Sprintf("mcp(%s/*)", m.Name)
		added, err := addAllowRule(path, rule)
		if err != nil {
			warns = append(warns, fmt.Sprintf("could not add %s to %s (agy will soft-deny the fleet tools): %v", rule, path, err))
		} else if added {
			// A global file: say so, since it outlives the session.
			warns = append(warns, fmt.Sprintf("added %q to permissions.allow in %s (agy's global settings; left in place)", rule, path))
		}
	}
	return made, warns
}

// removeFleetTools undoes what setupFleetTools recorded in f: the server
// entry (and the file and .agents directory once empty) and the exclude
// line. Anything else in those files stays, and so does an entry that was
// changed since setup.
func removeFleetTools(f *fleetSetup) error {
	root, err := os.OpenRoot(f.cwd)
	if err != nil {
		return err
	}
	defer root.Close()
	b, err := root.ReadFile(workspaceMCP)
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return errors.Join(err, removeExcludeLine(f.exclude, f.line))
	}
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s is no longer valid JSON; leaving it: %v", workspaceMCP, err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if cur, ok := servers[f.name]; ok {
		if !sameJSON(cur, f.installed) {
			// Changed since setup, by the user or another tool: theirs now.
			return nil
		}
		if f.previous != nil {
			servers[f.name] = f.previous // the entry it replaced
		} else {
			delete(servers, f.name)
		}
		if f.fileCreated && len(servers) == 0 && len(doc) == 1 {
			err = root.Remove(workspaceMCP)
			_ = root.Remove(filepath.Dir(workspaceMCP)) // only if now empty
		} else {
			var out []byte
			if out, err = json.MarshalIndent(doc, "", "  "); err == nil {
				err = writeAtomicIn(root, workspaceMCP, append(out, '\n'))
			}
		}
	}
	return errors.Join(err, removeExcludeLine(f.exclude, f.line))
}

// removeExcludeLine drops line, which gitExclude added, from the exclude
// file ex.
func removeExcludeLine(ex excludeFile, line string) error {
	if ex.root == "" {
		return nil
	}
	root, err := os.OpenRoot(ex.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	fi, err := root.Lstat(ex.rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Join(ex.root, ex.rel))
	}
	cur, err := root.ReadFile(ex.rel)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(cur), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == line {
			return writeAtomicIn(root, ex.rel, []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), "")))
		}
	}
	return nil
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
// file, whether the entry is new, and the entry it replaced (nil if none,
// or if the entry already matched and nothing was written). All access goes through an os.Root on cwd, so a symlink the agent
// swaps in after insideWorkspace looked cannot lead the write outside.
func mergeMCPConfig(cwd, rel, name string, server map[string]any) (created, added bool, previous map[string]any, err error) {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return false, false, nil, err
	}
	defer root.Close()
	path := filepath.Join(cwd, rel)
	doc := map[string]any{}
	b, err := root.ReadFile(rel)
	created = os.IsNotExist(err)
	switch {
	case created:
	case err != nil:
		return false, false, nil, err
	case len(bytes.TrimSpace(b)) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, false, nil, fmt.Errorf("%s is not valid JSON; not touching it: %v", path, err)
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		if _, exists := doc["mcpServers"]; exists {
			return false, false, nil, fmt.Errorf("%s: mcpServers is not an object; not touching it", path)
		}
		servers = map[string]any{}
	}
	old, replacing := servers[name].(map[string]any)
	if replacing {
		same := old["command"] == server["command"] && reflect.DeepEqual(asStrings(old["args"]), server["args"])
		if same {
			return false, false, nil, nil
		}
		// Keep fields the user added (disabledTools, cwd, ...).
		for k, v := range old {
			if _, set := server[k]; !set && k != "env" {
				server[k] = v
			}
		}
	}
	_, existed := servers[name]
	if replacing {
		previous = old
	}
	servers[name] = server
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, false, nil, err
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return false, false, nil, err
	}
	return created, !existed, previous, writeAtomicIn(root, rel, append(out, '\n'))
}

// sameJSON reports whether a and b encode to the same JSON (a value read
// from a file and one written as Go maps and slices compare equal).
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
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

// addAllowRule adds rule to permissions.allow in agy's settings.json and
// reports whether it changed the file.
func addAllowRule(path, rule string) (bool, error) {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return false, err
	case len(bytes.TrimSpace(b)) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("not valid JSON; not touching it: %v", err)
		}
	}
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		if _, exists := doc["permissions"]; exists {
			return false, fmt.Errorf("permissions is not an object; not touching it")
		}
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	for _, r := range allow {
		if r == rule || r == "mcp(*)" {
			return false, nil
		}
	}
	perms["allow"] = append(allow, rule)
	doc["permissions"] = perms
	out, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, writeAtomic(path, append(out, '\n'))
}

// excludeFile is a .git/info/exclude that gitExclude added a line to: rel
// inside the directory root.
type excludeFile struct{ root, rel string }

// gitExclude lists rel in the enclosing repository's .git/info/exclude so a
// file Switchworks created does not show up in git status. It returns the
// exclude file and the line when it added one.
//
// The workspace controls .git, and with a .git file its "gitdir:" and
// commondir. So that git directory must lie inside the directory holding
// the .git file, or (a submodule or nested worktree) inside the .git
// directory of the repository enclosing it, and it is reached through an
// os.Root there, so no symlink leads out. A worktree whose git directory is
// elsewhere gets no exclude line.
func gitExclude(cwd, rel string) (excludeFile, string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return excludeFile{}, "", err
	}
	dir := abs
	var fi os.FileInfo
	for {
		if fi, err = os.Lstat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return excludeFile{}, "", nil // not in a repository
		}
		dir = parent
	}
	top, gitDir := dir, ".git"
	if fi.Mode().IsRegular() {
		// A worktree or submodule: .git is a "gitdir: <path>" file.
		b, err := os.ReadFile(filepath.Join(dir, ".git"))
		if err != nil {
			return excludeFile{}, "", err
		}
		p := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:"))
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		top = enclosingRepo(dir)
		var ok bool
		if gitDir, ok = allowedGitDir(dir, top, p); !ok {
			return excludeFile{}, "", fmt.Errorf("%s points outside the repository", filepath.Join(dir, ".git"))
		}
	}
	root, err := os.OpenRoot(top)
	if err != nil {
		return excludeFile{}, "", err
	}
	defer root.Close()
	// Only write into what is really a git directory; a worktree's
	// excludes live in its common directory.
	if !isGitDir(root, gitDir) {
		return excludeFile{}, "", fmt.Errorf("%s is not a git directory", filepath.Join(top, gitDir))
	}
	if b, err := root.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(top, gitDir, c)
		}
		if cr, ok := allowedGitDir(dir, top, c); ok && isGitDir(root, cr) {
			gitDir = cr
		}
	}
	r, err := filepath.Rel(dir, filepath.Join(abs, rel))
	if err != nil {
		return excludeFile{}, "", err
	}
	r = "/" + filepath.ToSlash(r)
	info := filepath.Join(gitDir, "info")
	ex := filepath.Join(info, "exclude")
	cur, _ := root.ReadFile(ex)
	for _, l := range strings.Split(string(cur), "\n") {
		if strings.TrimSpace(l) == r {
			return excludeFile{}, "", nil
		}
	}
	if err := root.Mkdir(info, 0o755); err != nil && !os.IsExist(err) {
		return excludeFile{}, "", err
	}
	if fi, err := root.Lstat(info); err != nil || !fi.IsDir() {
		return excludeFile{}, "", fmt.Errorf("%s is not a directory", filepath.Join(top, info))
	}
	if fi, err := root.Lstat(ex); err == nil && !fi.Mode().IsRegular() {
		return excludeFile{}, "", fmt.Errorf("%s is not a regular file", filepath.Join(top, ex))
	}
	f, err := root.OpenFile(ex, os.O_APPEND|os.O_CREATE|os.O_WRONLY|oNoFollow, 0o644)
	if err != nil {
		return excludeFile{}, "", err
	}
	defer f.Close()
	prefix := ""
	if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
		prefix = "\n"
	}
	if _, err := fmt.Fprintf(f, "%s%s\n", prefix, r); err != nil {
		return excludeFile{}, "", err
	}
	return excludeFile{root: top, rel: ex}, r, nil
}

// allowedGitDir returns p relative to top when a .git file in dir may lead
// there: inside dir, or inside top's .git when top encloses dir.
func allowedGitDir(dir, top, p string) (string, bool) {
	_, inDir := relInside(dir, p)
	_, inTopGit := relInside(filepath.Join(top, ".git"), p)
	if !inDir && (top == dir || !inTopGit) {
		return "", false
	}
	return relInside(top, p)
}

// enclosingRepo returns the nearest directory above dir whose .git is a git
// directory (the superproject of a submodule, or the repository a worktree
// is nested in), or dir when there is none.
func enclosingRepo(dir string) string {
	for a := filepath.Dir(dir); ; a = filepath.Dir(a) {
		if fi, err := os.Lstat(filepath.Join(a, ".git")); err == nil && fi.IsDir() {
			if r, err := os.OpenRoot(a); err == nil {
				ok := isGitDir(r, ".git")
				r.Close()
				if ok {
					return a
				}
			}
		}
		if filepath.Dir(a) == a {
			return dir
		}
	}
}

// relInside returns p relative to dir when p (as text) is inside dir,
// trying dir with its symlinks resolved too (git writes resolved paths).
func relInside(dir, p string) (string, bool) {
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
	}
	for _, d := range dirs {
		if r, err := filepath.Rel(d, p); err == nil && filepath.IsLocal(r) {
			return r, true
		}
	}
	return "", false
}

// isGitDir reports whether dir (inside root) looks like a git directory: a
// HEAD file and an objects directory (or, for a worktree, a commondir file).
func isGitDir(root *os.Root, dir string) bool {
	if fi, err := root.Stat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi, err := root.Stat(filepath.Join(dir, "objects")); err == nil && fi.IsDir() {
		return true
	}
	_, err := root.Stat(filepath.Join(dir, "commondir"))
	return err == nil
}

func writeAtomic(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".swk-*")
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
	tmp := filepath.Join(filepath.Dir(rel), ".swk-"+hex.EncodeToString(rnd[:]))
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
