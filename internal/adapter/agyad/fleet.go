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
	"regexp"
	"sort"
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
	previous    any            // the entry it replaced, restored on cleanup
	exclude     excludeFile    // .git/info/exclude, if a line was added to it
	line        string         // the line added
	settings    string         // agy's settings.json, if rule was added to it
	rule        string         // the allow rule added
}

// newFleetName returns a server name for one workspace's fleet tools: base
// and 16 random hex digits. Only that name is pre-approved in agy's global
// settings, so a server another workspace defines (as "swk", say) is not,
// and nothing can define this one ahead of time.
func newFleetName(base string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return base + "-" + hex.EncodeToString(b[:])
}

// fleetNameRe matches the names newFleetName makes from base.
func fleetNameRe(base string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `-[0-9a-f]{16}$`)
}

// fleetServer is the workspace entry for m: its command and args, nothing
// else (no env: credentials reach `swk mcp` through agy's environment).
func fleetServer(m *adapter.MCPServer) map[string]any {
	cmd := m.Command
	if abs, err := proc.LookPath(cmd); err == nil {
		cmd = abs
	}
	args := m.Args
	if args == nil {
		args = []string{}
	}
	return map[string]any{"command": cmd, "args": args}
}

// settingsPath is agy's global settings.json.
func settingsPath(cfg Config) string {
	if cfg.Options.SettingsPath != "" {
		return cfg.Options.SettingsPath
	}
	home := cfg.Env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// setupFleetTools makes the fleet tools available to agy sessions in
// spec.Cwd under a new name and returns warnings for anything it could not
// do, plus a record of what it added (nil when it added nothing).
// It never removes or rewrites other entries, and never writes credentials
// to disk.
func setupFleetTools(spec adapter.StartSpec, cfg Config) (*fleetSetup, []string) {
	m := spec.MCP
	var warns []string
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "."
	}
	if err := insideWorkspace(cwd, workspaceMCP); err != nil {
		return nil, append(warns, "fleet tools unavailable: "+err.Error())
	}
	name := newFleetName(m.Name)
	server := fleetServer(m)
	created, added, previous, err := mergeMCPConfig(cwd, workspaceMCP, name, server)
	if err != nil {
		return nil, append(warns, "fleet tools unavailable: "+err.Error())
	}
	if !added && previous == nil {
		return nil, warns // already there as wanted; not this session's
	}
	made := &fleetSetup{cwd: cwd, name: name, fileCreated: created, installed: server, previous: previous}
	if created {
		if made.exclude, made.line, err = gitExclude(cwd, workspaceMCP); err != nil {
			warns = append(warns, "could not add "+workspaceMCP+" to .git/info/exclude: "+err.Error())
		}
	}
	return made, append(warns, addFleetRule(made, spec, cfg)...)
}

// adoptFleetTools takes over, for a resumed session, the entry an earlier
// node set up in spec.Cwd: one with a name newFleetName makes and exactly
// this node's command and args, which so runs nothing but `swk mcp`. It
// adds the entry's allow rule again (the earlier node removed it, or the
// sweep did). It writes nothing to the workspace, and returns nil when
// there is no such entry.
func adoptFleetTools(spec adapter.StartSpec, cfg Config) (*fleetSetup, []string) {
	m := spec.MCP
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "."
	}
	if insideWorkspace(cwd, workspaceMCP) != nil {
		return nil, nil
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, nil
	}
	defer root.Close()
	b, err := root.ReadFile(workspaceMCP)
	if err != nil {
		return nil, nil
	}
	doc := map[string]any{}
	if json.Unmarshal(b, &doc) != nil {
		return nil, nil
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	server := fleetServer(m)
	re := fleetNameRe(m.Name)
	var names []string
	for n, v := range servers {
		if re.MatchString(n) && sameJSON(v, server) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	made := &fleetSetup{cwd: cwd, name: names[0], installed: server,
		// Nothing else in the file: the earlier node created it.
		fileCreated: len(servers) == 1 && len(doc) == 1}
	return made, addFleetRule(made, spec, cfg)
}

// addFleetRule adds the allow rule for f's server to agy's settings unless
// the session skips permissions anyway, and records it in f.
func addFleetRule(f *fleetSetup, spec adapter.StartSpec, cfg Config) []string {
	if skipsPermissions(spec) || f.rule != "" {
		return nil
	}
	path := settingsPath(cfg)
	rule := fmt.Sprintf("mcp(%s/*)", f.name)
	added, err := addAllowRule(path, rule)
	if err != nil {
		return []string{fmt.Sprintf("could not add %s to %s (agy will soft-deny the fleet tools): %v", rule, path, err)}
	}
	if !added {
		return nil
	}
	f.settings, f.rule = path, rule
	// A global file: say so, though the rule goes with the sessions.
	return []string{fmt.Sprintf("added %q to permissions.allow in %s (agy's global settings) while sessions here run", rule, path)}
}

// removeFleetRule takes out the allow rule addFleetRule added for f.
func removeFleetRule(f *fleetSetup) error {
	if f.rule == "" {
		return nil
	}
	rule := f.rule
	if _, err := removeAllowRules(f.settings, func(r string) bool { return r == rule }); err != nil {
		return err
	}
	f.settings, f.rule = "", ""
	return nil
}

// sweepRules runs once per adapter, before its first fleet tools setup: it
// takes out allow rules for fleet tools servers that no session here uses
// (left by a node that crashed), and warns about a rule for the fixed name
// that earlier versions added. Callers hold fleetMu.
func (a *Adapter) sweepRules(base string) []string {
	if a.swept {
		return nil
	}
	a.swept = true
	path := settingsPath(a.cfg)
	live := map[string]bool{}
	for _, o := range a.owners {
		if o.setup.rule != "" {
			live[o.setup.rule] = true
		}
	}
	re := fleetNameRe(base)
	legacy := fmt.Sprintf("mcp(%s/*)", base)
	var warns []string
	_, err := removeAllowRules(path, func(r string) bool {
		if r == legacy {
			warns = append(warns, fmt.Sprintf("permissions.allow in %s has %q (which earlier Switchworks versions added): it pre-approves any MCP server named %s, including one an untrusted workspace defines; consider removing it",
				path, legacy, base))
		}
		n, ok := strings.CutPrefix(r, "mcp(")
		if n, ok2 := strings.CutSuffix(n, "/*)"); ok && ok2 {
			return re.MatchString(n) && !live[r]
		}
		return false
	})
	if err != nil {
		a.cfg.Logf("agy: removing stale fleet tools rules from %s: %v", path, err)
	}
	return warns
}

// removeFleetTools undoes what setupFleetTools recorded in f: the allow
// rule, the server entry (and the file and .agents directory once empty)
// and the exclude line. Anything else in those files stays, and so does an
// entry that was changed since setup.
func removeFleetTools(f *fleetSetup) error {
	return errors.Join(removeFleetRule(f), removeWorkspaceEntry(f))
}

func removeWorkspaceEntry(f *fleetSetup) error {
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
// under cwd to exactly server, keeping everything else. It reports whether
// it created the file, whether the entry is new, and the entry it replaced
// (nil if none, or if the entry already matched and nothing was written).
// An entry with other fields (env, say: LD_PRELOAD would run code in
// `swk mcp`) does not match, and none of them is carried over. All access
// goes through an os.Root on cwd, so a symlink the agent swaps in after
// insideWorkspace looked cannot lead the write outside.
func mergeMCPConfig(cwd, rel, name string, server map[string]any) (created, added bool, previous any, err error) {
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
	old, existed := servers[name]
	if existed && sameJSON(old, server) {
		return false, false, nil, nil
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
	return created, !existed, old, writeAtomicIn(root, rel, append(out, '\n'))
}

// sameJSON reports whether a and b encode to the same JSON (a value read
// from a file and one written as Go maps and slices compare equal).
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// readSettings reads agy's settings.json at path (an empty document if
// there is none) and its permissions.allow.
func readSettings(path string) (doc, perms map[string]any, allow []any, err error) {
	doc = map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, nil, nil, err
	case len(bytes.TrimSpace(b)) > 0:
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, nil, nil, fmt.Errorf("not valid JSON; not touching it: %v", err)
		}
	}
	perms, _ = doc["permissions"].(map[string]any)
	if perms == nil {
		if _, exists := doc["permissions"]; exists {
			return nil, nil, nil, fmt.Errorf("permissions is not an object; not touching it")
		}
		perms = map[string]any{}
	}
	allow, _ = perms["allow"].([]any)
	return doc, perms, allow, nil
}

// writeSettings writes doc to agy's settings.json at path, atomically and
// (if new) readable by the user only.
func writeSettings(path string, doc map[string]any) error {
	out, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, append(out, '\n'))
}

// addAllowRule adds rule to permissions.allow in agy's settings.json and
// reports whether it changed the file.
func addAllowRule(path, rule string) (bool, error) {
	doc, perms, allow, err := readSettings(path)
	if err != nil {
		return false, err
	}
	for _, r := range allow {
		if r == rule || r == "mcp(*)" {
			return false, nil
		}
	}
	perms["allow"] = append(allow, rule)
	doc["permissions"] = perms
	return true, writeSettings(path, doc)
}

// removeAllowRules takes the rules drop picks out of permissions.allow in
// agy's settings.json and reports whether it changed the file. A missing
// file is no error.
func removeAllowRules(path string, drop func(string) bool) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	doc, perms, allow, err := readSettings(path)
	if err != nil {
		return false, err
	}
	keep := make([]any, 0, len(allow))
	for _, r := range allow {
		if s, ok := r.(string); !ok || !drop(s) {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(allow) {
		return false, nil
	}
	perms["allow"] = keep
	doc["permissions"] = perms
	return true, writeSettings(path, doc)
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
	mode := os.FileMode(0o600) // a new file is the user's alone
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
