// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/proc"
)

// workspace_git: git run by the node for one session, outside its sandbox.
//
// Agents in a sandbox often cannot use git: on Windows .git is read-only
// and Git for Windows' sh.exe fails under the restricted token, and on
// Linux the profile keeps .git/config read-only. The node runs git for
// them instead, on the session's own repository only (the one its working
// directory is in), with the agent's influence cut down to the work tree:
//
//   - The repository must be the one the session's cwd is in, with a .git
//     directory at its top, or a .git file of a normal linked worktree
//     (whose gitdir is <common>/worktrees/<name> and points back). git
//     gets GIT_DIR and GIT_WORK_TREE, so it does no discovery of its own.
//   - The repository's own config is trusted (it is the owner's: agents
//     cannot write .git/config on Windows or Linux), but it may not
//     include other files or name filter, merge or diff drivers, which
//     would run commands the work tree selects through .gitattributes.
//   - Every git gets -c overrides for the keys that run commands: hooks
//     (an empty directory of the node's), fsmonitor, askpass, editors,
//     signing, credential helpers. Only https is spoken (protocol.allow);
//     a GitHub ssh remote is fetched and pushed over https with the agent
//     token (url.<https>.insteadOf on git's command line; other ssh hosts
//     are rewritten the same way by httpsURL), so the owner's ssh keys and
//     credential manager are never used.
//   - Push needs node.json's git.gh_config_dir (a gh config holding the
//     agent token) and is refused for main, master and the remote's
//     default branch; never forced.
//
// Uncommitted work is saved (stash-like commit in refs/stash, and a binary
// patch under the node's directory) before a sync touches the work tree.

const opGit = "git"

// Per-op time limits.
var gitTimeouts = map[string]time.Duration{
	"status": time.Minute, "commit": 2 * time.Minute, "sync": 5 * time.Minute,
	"push": 5 * time.Minute, "branch_delete": 2 * time.Minute,
}

// GitArgs are workspace_git's arguments.
type GitArgs struct {
	Op        string   `json:"op"`
	Path      string   `json:"path,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Strategy  string   `json:"strategy,omitempty"`
	Message   string   `json:"message,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	NewBranch string   `json:"new_branch,omitempty"`
	// DeleteRemote (branch_delete) also deletes the branch on its remote,
	// if this tool pushed it.
	DeleteRemote bool `json:"delete_remote,omitempty"`
}

// GitStatus is a repository's state.
type GitStatus struct {
	Repo     string   `json:"repo"`
	Branch   string   `json:"branch,omitempty"`
	Detached bool     `json:"detached,omitempty"`
	Head     string   `json:"head,omitempty"`
	Upstream string   `json:"upstream,omitempty"`
	Ahead    int      `json:"ahead"`
	Behind   int      `json:"behind"`
	Clean    bool     `json:"clean"`
	Changed  []string `json:"changed,omitempty"` // "XY path", as git status --short
	// More counts changed files left out of Changed.
	More int `json:"more,omitempty"`
}

// GitResult is workspace_git's answer.
type GitResult struct {
	Op string `json:"op"`
	// Outcome: "ok", "up_to_date", "fast_forward", "rebased", "conflict"
	// (changes reapplied with conflict markers), "rebase_conflict" (the
	// rebase was aborted; nothing changed).
	Outcome   string     `json:"outcome"`
	Message   string     `json:"message,omitempty"`
	Onto      string     `json:"onto,omitempty"`
	Commit    string     `json:"commit,omitempty"`
	Author    string     `json:"author,omitempty"`
	Pushed    string     `json:"pushed,omitempty"`
	Conflicts []string   `json:"conflicts,omitempty"`
	Saved     *gitSaved  `json:"saved,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
	Status    *GitStatus `json:"status,omitempty"`
}

// gitSaved is where a sync saved uncommitted work.
type gitSaved struct {
	Commit string `json:"commit"`          // stash-like commit, also in refs/stash
	Patch  string `json:"patch,omitempty"` // binary patch under the node's directory
}

// gitRepo is a validated repository.
type gitRepo struct {
	top    string // work tree, symlinks resolved
	gitDir string // GIT_DIR
	common string // common dir (gitDir, or a linked worktree's main .git)
}

// errGit is an error the agent should see as is.
type errGit struct{ msg string }

func (e *errGit) Error() string { return e.msg }

func gitErrf(f string, a ...any) error { return &errGit{fmt.Sprintf(f, a...)} }

// handleGit runs one workspace_git op for session s.
func (n *Node) handleGit(ctx context.Context, s *session, body []byte) (*GitResult, error) {
	var a GitArgs
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("bad arguments: %w", err)
	}
	timeout, ok := gitTimeouts[a.Op]
	if !ok {
		return nil, gitErrf("unknown op %q: use status, sync, commit, push or branch_delete", a.Op)
	}
	s.mu.Lock()
	cwd, root, spath := s.rec.Spec.Cwd, s.rec.Spec.CwdRoot, s.rec.Path
	s.mu.Unlock()
	repo, err := findRepo(cwd, root, a.Path)
	if err != nil {
		return nil, err
	}
	g, err := n.newGit(ctx, repo, spath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	g.ctx = ctx
	if err := g.checkConfig(); err != nil {
		return nil, err
	}
	mu := n.gitLock(repo.common)
	mu.Lock()
	defer mu.Unlock()
	var res *GitResult
	switch a.Op {
	case "status":
		res = &GitResult{Outcome: "ok"}
	case "sync":
		res, err = g.sync(a)
	case "commit":
		res, err = g.commit(a, cwd)
	case "push":
		res, err = g.push(a)
	case "branch_delete":
		res, err = g.branchDelete(a)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, gitErrf("workspace_git %s timed out after %s: %v", a.Op, timeout, err)
		}
		return nil, err
	}
	res.Op = a.Op
	if st, err := g.status(); err == nil {
		res.Status = st
	} else {
		res.Warnings = append(res.Warnings, "status: "+err.Error())
	}
	return res, nil
}

func (n *Node) gitLock(key string) *sync.Mutex {
	n.gitMu.Lock()
	defer n.gitMu.Unlock()
	if n.gitLocks == nil {
		n.gitLocks = map[string]*sync.Mutex{}
	}
	mu := n.gitLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		n.gitLocks[key] = mu
	}
	return mu
}

// --- the repository ---

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether p is dir or inside it (both resolved).
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// findRepo finds and checks the repository the session's cwd is in. path,
// if set, must be inside cwd and in that same repository.
func findRepo(cwd, root, path string) (*gitRepo, error) {
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	if err := proc.CheckPinned(root, cwd); err != nil {
		return nil, err
	}
	rcwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	top, err := findTop(rcwd)
	if err != nil {
		return nil, err
	}
	if path != "" {
		p := path
		if !filepath.IsAbs(p) {
			p = filepath.Join(rcwd, p)
		}
		rp, err := filepath.EvalSymlinks(p)
		if err != nil || !within(rcwd, rp) {
			return nil, gitErrf("path %q is outside your working directory %s; workspace_git works only on your session's own repository", path, rcwd)
		}
		if t, err := findTop(rp); err != nil || !samePath(t, top) {
			return nil, gitErrf("path %q is not in your session's repository %s; workspace_git works only on that one", path, top)
		}
	}
	return checkGitDir(top)
}

// findTop walks up from dir to the first directory with a .git entry.
func findTop(dir string) (string, error) {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		up := filepath.Dir(d)
		if up == d {
			return "", gitErrf("%s is not in a git repository", dir)
		}
		d = up
	}
}

func readSmall(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 4096)
	k, _ := f.Read(b)
	return strings.TrimSpace(string(b[:k])), nil
}

func resolveFrom(base, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.EvalSymlinks(p)
}

// checkGitDir accepts top/.git as a real directory, or as the .git file of
// a normal linked worktree.
func checkGitDir(top string) (*gitRepo, error) {
	dotgit := filepath.Join(top, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return nil, err
	}
	refuse := func(why string) error {
		return gitErrf("refusing %s: %s", dotgit, why)
	}
	r := &gitRepo{top: top}
	switch {
	case fi.Mode().IsDir():
		real, err := filepath.EvalSymlinks(dotgit)
		if err != nil || !samePath(real, dotgit) {
			return nil, refuse("it resolves elsewhere")
		}
		if _, err := os.Lstat(filepath.Join(dotgit, "commondir")); err == nil {
			return nil, refuse("a .git directory with a commondir file is not a normal repository")
		}
		r.gitDir, r.common = dotgit, dotgit
	case fi.Mode().IsRegular():
		s, err := readSmall(dotgit)
		if err != nil {
			return nil, err
		}
		x, ok := strings.CutPrefix(s, "gitdir: ")
		if !ok {
			return nil, refuse("not a gitdir file")
		}
		gd, err := resolveFrom(top, strings.TrimSpace(x))
		if err != nil {
			return nil, refuse("its gitdir does not exist")
		}
		c, err := readSmall(filepath.Join(gd, "commondir"))
		if err != nil {
			return nil, refuse("its gitdir is not a linked worktree's (no commondir)")
		}
		common, err := resolveFrom(gd, c)
		if err != nil || !samePath(filepath.Dir(gd), filepath.Join(common, "worktrees")) {
			return nil, refuse("its gitdir is not <repository>/worktrees/<name>")
		}
		back, err := readSmall(filepath.Join(gd, "gitdir"))
		if err != nil {
			return nil, refuse("its gitdir does not point back to it")
		}
		if bp, err := resolveFrom(gd, back); err != nil || !samePath(bp, dotgit) {
			return nil, refuse("its gitdir belongs to another worktree")
		}
		r.gitDir, r.common = gd, common
	default:
		return nil, refuse("not a directory or a file (a symlink?)")
	}
	for _, p := range []string{"HEAD", "config"} {
		if fi, err := os.Lstat(filepath.Join(r.common, p)); err != nil || !fi.Mode().IsRegular() {
			return nil, refuse("not a git repository (no " + p + ")")
		}
	}
	if _, err := os.Lstat(filepath.Join(r.gitDir, "HEAD")); err != nil {
		return nil, refuse("not a git repository (no HEAD)")
	}
	return r, nil
}

// --- running git ---

type gitRunner struct {
	n       *Node
	ctx     context.Context
	bin     string
	repo    *gitRepo
	args    []string // global options and -c overrides
	env     []string
	host    string
	session string // the session's path, host/name
	canPush string // why push is not possible, or ""
}

// gitEnvDrop: environment variables never passed to git. GIT_* could
// redirect it or add config; GH_* and the token variables would let gh use
// the owner's token instead of the configured one.
func gitEnvDrop(k string) bool {
	u := strings.ToUpper(k)
	switch u {
	case "GIT_SSL_CAINFO", "GIT_SSL_CAPATH", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM":
		return false
	case "GITHUB_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SSH_ASKPASS", "SSH_AUTH_SOCK", "LC_ALL", "LANG":
		return true
	}
	return strings.HasPrefix(u, "GIT_") || strings.HasPrefix(u, "GH_") || strings.HasPrefix(u, "GCM_")
}

func (n *Node) newGit(ctx context.Context, repo *gitRepo, spath string) (*gitRunner, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, gitErrf("git is not on the node's PATH")
	}
	hooks := filepath.Join(n.dir, "git", "hooks-empty")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return nil, err
	}
	if ents, err := os.ReadDir(hooks); err != nil || len(ents) > 0 {
		return nil, fmt.Errorf("%s must be an empty directory", hooks)
	}
	host, _, _ := strings.Cut(spath, "/")
	g := &gitRunner{n: n, ctx: ctx, bin: bin, repo: repo, host: host, session: spath}
	slash := filepath.ToSlash
	ghBase := "https://github.com/"
	if n.gitGitHubBase != "" {
		ghBase = n.gitGitHubBase
	}
	g.args = []string{"--no-pager",
		"-c", "core.hooksPath=" + slash(hooks),
		"-c", "core.fsmonitor=false",
		"-c", "core.sshCommand=",
		"-c", "core.askPass=",
		"-c", "core.alternateRefsCommand=",
		"-c", "core.editor=:",
		"-c", "sequence.editor=:",
		"-c", "core.quotePath=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.file.allow=" + map[bool]string{false: "never", true: "always"}[n.gitAllowLocal],
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "push.recurseSubmodules=no",
		"-c", "push.followTags=false",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "push.gpgSign=false",
		"-c", "rebase.autoStash=false",
		"-c", "rebase.updateRefs=false",
		"-c", "merge.autoStash=false",
		"-c", "rerere.enabled=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "http.extraHeader=",
		"-c", "credential.helper=",
		// GitHub ssh remotes are reached over https, for workspace_git's own
		// git only (the repository's config is left alone): agents' hosts
		// often have no ssh key, and the owner's must not be used.
		"-c", "url." + ghBase + ".insteadOf=git@github.com:",
		"-c", "url." + ghBase + ".insteadOf=ssh://git@github.com/",
		"-c", "safe.directory=" + slash(repo.top),
		"-c", "safe.directory=" + slash(repo.gitDir),
		"-c", "safe.directory=" + slash(repo.common),
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !gitEnvDrop(k) {
			g.env = append(g.env, kv)
		}
	}
	g.env = append(g.env, "GIT_DIR="+repo.gitDir, "GIT_WORK_TREE="+repo.top,
		"GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "GCM_INTERACTIVE=never", "GH_PROMPT_DISABLED=1")
	dir := n.cfg.Git.GHConfigDir
	switch {
	case dir == "" && n.gitAllowLocal:
	case dir == "":
		g.canPush = `push needs an agent token: set "git": {"gh_config_dir": "<dir>"} in this host's node.json to a gh config directory whose hosts.yml holds a token scoped to the agents' repositories (see docs/harnesses.md, "Git for agents"), then restart the node`
	default:
		if d, err := expandHome(dir); err == nil {
			dir = d
		}
		gh, err := exec.LookPath("gh")
		if fi, serr := os.Stat(filepath.Join(dir, "hosts.yml")); serr != nil || !fi.Mode().IsRegular() {
			g.canPush = fmt.Sprintf("node.json git.gh_config_dir %s has no hosts.yml (log gh in there with the agent token: GH_CONFIG_DIR=%s gh auth login --with-token)", dir, dir)
			break
		}
		if err != nil {
			g.canPush = "gh is not on the node's PATH; workspace_git uses `gh auth git-credential` for the agent token"
			break
		}
		if strings.ContainsAny(gh, `'"`) {
			g.canPush = "gh's path has quotes in it"
			break
		}
		g.args = append(g.args, "-c", "credential.helper=!'"+slash(gh)+"' auth git-credential")
		g.env = append(g.env, "GH_CONFIG_DIR="+dir)
	}
	return g, nil
}

// gitOut is one git's result.
type gitOut struct {
	out  string
	err  string
	code int
}

// exec runs git with args, stdin and extra environment. A non-zero exit is
// not an error; failing to run git is.
func (g *gitRunner) exec(stdin []byte, env []string, args ...string) (gitOut, error) {
	cmd := exec.CommandContext(g.ctx, g.bin, append(slices.Clone(g.args), args...)...)
	cmd.Dir = g.repo.top
	cmd.Env = append(slices.Clone(g.env), env...)
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &capWriter{b: &out, max: 32 << 20}, &capWriter{b: &errb, max: 64 << 10}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	o := gitOut{out: out.String(), err: strings.TrimSpace(errb.String())}
	var ee *exec.ExitError
	if errors.As(err, &ee) && g.ctx.Err() == nil {
		o.code = ee.ExitCode()
		return o, nil
	}
	return o, err
}

type capWriter struct {
	b   *bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.b.Len(); room > 0 {
		w.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// run is exec that fails on a non-zero exit, and returns stdout trimmed.
func (g *gitRunner) run(args ...string) (string, error) {
	return g.runWith(nil, nil, args...)
}

func (g *gitRunner) runWith(stdin []byte, env []string, args ...string) (string, error) {
	o, err := g.exec(stdin, env, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	if o.code != 0 {
		msg := o.err
		if msg == "" {
			msg = strings.TrimSpace(o.out)
		}
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		return "", fmt.Errorf("git %s failed (exit %d): %s", args[0], o.code, msg)
	}
	return strings.TrimSpace(o.out), nil
}

// riskyKeyRe matches repository config keys refused: includes (another
// file, possibly in the work tree) and drivers the work tree selects
// through .gitattributes, which run commands.
var riskyKeyRe = regexp.MustCompile(`(?i)^(include\.path|includeif\..*\.path|filter\..*\.(clean|smudge|process)|merge\..*\.driver|diff\..*\.(command|textconv))$`)

// checkConfig refuses a repository whose own config has risky keys.
func (g *gitRunner) checkConfig() error {
	files := []string{filepath.Join(g.repo.common, "config")}
	if wt := filepath.Join(g.repo.gitDir, "config.worktree"); g.repo.gitDir != g.repo.common {
		files = append(files, wt)
	} else {
		files = append(files, filepath.Join(g.repo.common, "config.worktree"))
	}
	for _, f := range files {
		if _, err := os.Lstat(f); err != nil {
			continue
		}
		out, err := g.run("config", "--no-includes", "--file", f, "--null", "--list")
		if err != nil {
			return err
		}
		for _, kv := range strings.Split(out, "\x00") {
			k, _, _ := strings.Cut(kv, "\n")
			if riskyKeyRe.MatchString(k) {
				return gitErrf("refusing the repository: its config %s sets %s, which could run a command the work tree chooses; run git yourself there, or remove it", f, k)
			}
		}
	}
	return nil
}

// --- identity ---

func validIdent(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > 200 {
		return false
	}
	if strings.ContainsAny(s, "<>\n\r\x00`") || strings.Contains(s, "$(") || strings.Contains(s, "${") {
		return false
	}
	return true
}

// identity is the author: the repository's or the user's user.name and
// user.email, or a default naming the session.
func (g *gitRunner) identity() (name, email string) {
	name, _ = g.run("config", "--get", "user.name")
	email, _ = g.run("config", "--get", "user.email")
	if !validIdent(name) {
		name = g.session
	}
	if !validIdent(email) || !strings.Contains(email, "@") {
		email = "noreply@switchworks.invalid"
	}
	return name, email
}

func (g *gitRunner) identEnv() []string {
	name, email := g.identity()
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
}

// --- status ---

const maxChanged = 200

func (g *gitRunner) status() (*GitStatus, error) {
	out, err := g.run("status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	st := &GitStatus{Repo: g.repo.top}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		switch {
		case strings.HasPrefix(r, "# branch.oid "):
			st.Head = strings.TrimPrefix(r, "# branch.oid ")
		case strings.HasPrefix(r, "# branch.head "):
			st.Branch = strings.TrimPrefix(r, "# branch.head ")
			if st.Branch == "(detached)" {
				st.Branch, st.Detached = "", true
			}
		case strings.HasPrefix(r, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(r, "# branch.upstream ")
		case strings.HasPrefix(r, "# branch.ab "):
			var a, b int
			if _, err := fmt.Sscanf(strings.TrimPrefix(r, "# branch.ab "), "+%d -%d", &a, &b); err == nil {
				st.Ahead, st.Behind = a, b
			}
		case len(r) > 2 && (r[0] == '1' || r[0] == '2' || r[0] == 'u'):
			n := map[byte]int{'1': 9, '2': 10, 'u': 11}[r[0]]
			f := strings.SplitN(r, " ", n)
			if len(f) == n {
				st.addChanged(f[1] + " " + f[n-1])
			}
			if r[0] == '2' {
				i++ // the original path
			}
		case strings.HasPrefix(r, "? "):
			st.addChanged("?? " + r[2:])
		}
	}
	st.Clean = len(st.Changed) == 0 && st.More == 0
	return st, nil
}

func (st *GitStatus) addChanged(s string) {
	if len(st.Changed) < maxChanged {
		st.Changed = append(st.Changed, s)
	} else {
		st.More++
	}
}

// --- refs and remotes ---

func (g *gitRunner) oid(rev string) (string, error) {
	return g.run("rev-parse", "--verify", "-q", "--end-of-options", rev+"^{commit}")
}

func (g *gitRunner) currentBranch() (string, error) {
	b, err := g.run("symbolic-ref", "-q", "--short", "HEAD")
	if err != nil || b == "" {
		return "", gitErrf("HEAD is detached; check out a branch first")
	}
	return b, nil
}

// checkBranchName refuses names git would not take as a branch, and the
// forms that mean something else (@{-1}, -x, HEAD).
func (g *gitRunner) checkBranchName(b string) error {
	if b == "" || b == "HEAD" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "+") || strings.Contains(b, "@{") || strings.HasPrefix(b, "refs/") {
		return gitErrf("invalid branch name %q", b)
	}
	if _, err := g.run("check-ref-format", "refs/heads/"+b); err != nil {
		return gitErrf("invalid branch name %q", b)
	}
	return nil
}

func (g *gitRunner) checkRemoteName(r string) error {
	if r == "" || strings.HasPrefix(r, "-") || strings.ContainsAny(r, "/:\\ ") {
		return gitErrf("invalid remote name %q", r)
	}
	if _, err := g.run("check-ref-format", "refs/remotes/"+r+"/x"); err != nil {
		return gitErrf("invalid remote name %q", r)
	}
	if _, err := g.run("config", "--get", "remote."+r+".url"); err != nil {
		return gitErrf("no remote %q in this repository", r)
	}
	return nil
}

var scpLikeRe = regexp.MustCompile(`^(?:[^@/]+@)?([A-Za-z0-9.-]+):(.+)$`)

// httpsURL turns a remote's URL into the https one workspace_git talks to:
// https as is, ssh (ssh:// or user@host:path) to https on the same host and
// path. Other transports are refused (a local path only in tests).
func httpsURL(u string, allowLocal bool) (string, error) {
	switch {
	case strings.Contains(u, "::"):
		return "", gitErrf("remote URL %q uses a remote helper; workspace_git speaks https only", u)
	case strings.HasPrefix(u, "https://"):
		return u, nil
	case strings.HasPrefix(u, "ssh://"), strings.HasPrefix(u, "git+ssh://"), strings.HasPrefix(u, "ssh+git://"):
		rest := u[strings.Index(u, "://")+3:]
		hostport, p, ok := strings.Cut(rest, "/")
		if !ok || p == "" {
			break
		}
		if i := strings.LastIndex(hostport, "@"); i >= 0 {
			hostport = hostport[i+1:]
		}
		host := hostport
		if h, _, ok := strings.Cut(hostport, ":"); ok {
			host = h
		}
		return "https://" + host + "/" + strings.TrimPrefix(p, "~"), nil
	case filepath.IsAbs(u) || strings.HasPrefix(u, "file://"):
		if allowLocal {
			return u, nil
		}
	case !strings.Contains(u, "://"):
		if m := scpLikeRe.FindStringSubmatch(u); m != nil && len(m[1]) > 1 {
			return "https://" + m[1] + "/" + strings.TrimPrefix(m[2], "/"), nil
		}
	}
	return "", gitErrf("remote URL %q: workspace_git speaks https only (ssh remotes are reached over https)", u)
}

// remoteURL is the URL workspace_git fetches from or pushes to: the
// remote's, after the insteadOf rewrites (git remote get-url applies them),
// then httpsURL.
func (g *gitRunner) remoteURL(remote string, push bool) (string, error) {
	args := []string{"remote", "get-url"}
	if push {
		args = append(args, "--push")
	}
	u, err := g.run(append(args, remote)...)
	if err != nil {
		return "", err
	}
	return httpsURL(u, g.n.gitAllowLocal)
}

// remoteHead asks the remote for its default branch.
func (g *gitRunner) remoteHead(url string) (string, error) {
	out, err := g.run("ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		if r, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
			b, _, _ := strings.Cut(r, "\t")
			return b, nil
		}
	}
	return "", gitErrf("the remote %s reports no default branch", url)
}

// upstream is the branch's configured remote and remote branch, if any.
func (g *gitRunner) upstream(branch string) (remote, rbranch string) {
	remote, _ = g.run("config", "--get", "branch."+branch+".remote")
	merge, _ := g.run("config", "--get", "branch."+branch+".merge")
	rbranch, ok := strings.CutPrefix(merge, "refs/heads/")
	if remote == "" || remote == "." || !ok {
		return "", ""
	}
	return remote, rbranch
}

func (g *gitRunner) fetch(remote, url string) error {
	_, err := g.run("fetch", "--no-recurse-submodules", "--no-write-fetch-head", "--quiet", url,
		"+refs/heads/*:refs/remotes/"+remote+"/*")
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	return nil
}

// --- sync ---

// inProgress refuses a repository with a merge, rebase and the like going
// on, or unmerged files.
func (g *gitRunner) inProgress() error {
	for _, f := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer"} {
		if _, err := os.Lstat(filepath.Join(g.repo.gitDir, f)); err == nil {
			return gitErrf("a merge, rebase or cherry-pick is in progress (%s); finish or abort it first", f)
		}
	}
	if out, err := g.run("ls-files", "-u", "-z"); err != nil {
		return err
	} else if out != "" {
		return gitErrf("the index has unmerged files; resolve them (or git add them) first")
	}
	return nil
}

type gitSnapshot struct {
	commit    string // stash-like: tree = work tree, parents base and index
	indexTree string
	untracked []string
	saved     *gitSaved
}

// save snapshots tracked changes and untracked files that are not ignored,
// as a stash-like commit kept in refs/stash and a patch under the node's
// directory. It returns nil for a clean work tree.
func (g *gitRunner) save(base, branch string, ident []string) (*gitSnapshot, error) {
	ls, err := g.run("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var untracked []string
	for _, p := range strings.Split(ls, "\x00") {
		if p != "" && !strings.HasSuffix(p, "/") { // an embedded repository stays as it is
			untracked = append(untracked, p)
		}
	}
	idxTree, err := g.run("write-tree")
	if err != nil {
		return nil, err
	}
	tmpDir := filepath.Join(g.n.dir, "git", "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	tmpIdx := filepath.Join(tmpDir, "index-"+randHex())
	defer os.Remove(tmpIdx)
	idxPath, err := g.run("rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(idxPath); err == nil {
		if err := os.WriteFile(tmpIdx, b, 0o600); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIdx, "GIT_LITERAL_PATHSPECS=1"}
	if _, err := g.runWith(nil, env, "add", "-u"); err != nil {
		return nil, err
	}
	if len(untracked) > 0 {
		in := []byte(strings.Join(untracked, "\x00"))
		if _, err := g.runWith(in, env, "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return nil, err
		}
	}
	tree, err := g.runWith(nil, env, "write-tree")
	if err != nil {
		return nil, err
	}
	headTree, err := g.run("rev-parse", base+"^{tree}")
	if err != nil {
		return nil, err
	}
	if tree == headTree && idxTree == headTree {
		return nil, nil
	}
	when := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	icommit, err := g.runWith(nil, ident, "commit-tree", idxTree, "-p", base, "-m", "index on "+branch+": saved by workspace_git sync "+when)
	if err != nil {
		return nil, err
	}
	msg := "workspace_git sync of " + branch + " " + when + ": uncommitted changes"
	commit, err := g.runWith(nil, ident, "commit-tree", tree, "-p", base, "-p", icommit, "-m", msg)
	if err != nil {
		return nil, err
	}
	snap := &gitSnapshot{commit: commit, indexTree: idxTree, untracked: untracked, saved: &gitSaved{Commit: commit}}
	if _, err := g.run("stash", "store", "-m", msg, commit); err != nil {
		// Still reachable from the patch and the commit id; keep going
		// only if the patch below is written.
		snap.saved.Commit = commit + " (not in refs/stash: " + err.Error() + ")"
	}
	p, err := g.writePatch(base, commit)
	if err != nil {
		return nil, fmt.Errorf("saving the uncommitted changes: %w", err)
	}
	snap.saved.Patch = p
	return snap, nil
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writePatch keeps a binary patch of base..commit under the node's
// directory, the newest 50 per repository.
func (g *gitRunner) writePatch(base, commit string) (string, error) {
	sum := sha256.Sum256([]byte(g.repo.top))
	dir := filepath.Join(g.n.dir, "git", "saved", hex.EncodeToString(sum[:6]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	o, err := g.exec(nil, nil, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-color", base, commit)
	if err != nil || o.code != 0 {
		return "", fmt.Errorf("git diff: %v %s", err, o.err)
	}
	p := filepath.Join(dir, time.Now().UTC().Format("20060102T150405Z")+"-"+commit[:12]+".patch")
	if err := paths.WriteFile(p, []byte(o.out), 0o600); err != nil {
		return "", err
	}
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 50 {
		for _, e := range ents[:len(ents)-50] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return p, nil
}

// clear resets the work tree to HEAD and removes the untracked files the
// snapshot holds.
func (g *gitRunner) clear(snap *gitSnapshot) error {
	if _, err := g.run("reset", "--hard", "-q", "--no-recurse-submodules"); err != nil {
		return err
	}
	for _, p := range snap.untracked {
		full := filepath.Join(g.repo.top, filepath.FromSlash(p))
		if !within(g.repo.top, full) {
			continue
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for d := filepath.Dir(full); d != g.repo.top && within(g.repo.top, d); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	return nil
}

// reapply merges the snapshot onto HEAD (base: the commit it was taken
// on), leaving conflict markers in conflicted files. Everything ends up
// unstaged, except files that were staged in full.
func (g *gitRunner) reapply(snap *gitSnapshot, base string, ident []string) ([]string, error) {
	o, err := g.exec(nil, ident, "cherry-pick", "--no-commit", "-m", "1", "--strategy=ort", snap.commit)
	if err != nil {
		return nil, err
	}
	conflicts, _ := g.run("diff", "--name-only", "--diff-filter=U", "-z")
	var files []string
	for _, f := range strings.Split(conflicts, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	_, _ = g.exec(nil, nil, "cherry-pick", "--quit")
	if o.code != 0 && len(files) == 0 {
		return nil, fmt.Errorf("git cherry-pick failed (exit %d): %s", o.code, o.err)
	}
	if _, err := g.run("reset", "-q", "--no-recurse-submodules"); err != nil {
		return files, err
	}
	if len(files) == 0 {
		// Stage again the files that were staged in full (index and work
		// tree agreed); partly staged ones stay unstaged.
		staged := g.names(base, snap.indexTree)
		partly := g.names(snap.indexTree, snap.commit+"^{tree}")
		var again []string
		for _, p := range staged {
			if !slices.Contains(partly, p) {
				again = append(again, p)
			}
		}
		if len(again) > 0 {
			_, _ = g.runWith([]byte(strings.Join(again, "\x00")), []string{"GIT_LITERAL_PATHSPECS=1"},
				"add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul")
		}
	}
	return files, nil
}

// names lists the paths that differ between two trees.
func (g *gitRunner) names(a, b string) []string {
	out, err := g.run("diff", "--name-only", "--no-renames", "-z", a, b)
	if err != nil {
		return nil
	}
	var l []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			l = append(l, p)
		}
	}
	return l
}

func (g *gitRunner) sync(a GitArgs) (*GitResult, error) {
	strategy := a.Strategy
	if strategy == "" {
		strategy = "rebase"
	}
	if strategy != "rebase" && strategy != "ff-only" {
		return nil, gitErrf("strategy must be rebase or ff-only")
	}
	branch, err := g.currentBranch()
	if err != nil {
		return nil, err
	}
	if err := g.inProgress(); err != nil {
		return nil, err
	}
	remote, target := a.Remote, a.Branch
	upRemote, upBranch := g.upstream(branch)
	if remote == "" {
		remote = upRemote
		if remote == "" || (a.Branch != "" && a.Branch != upBranch) {
			remote = "origin"
		}
	}
	if err := g.checkRemoteName(remote); err != nil {
		return nil, err
	}
	url, err := g.remoteURL(remote, false)
	if err != nil {
		return nil, err
	}
	if target == "" && remote == upRemote {
		target = upBranch
	}
	if target == "" {
		if target, err = g.remoteHead(url); err != nil {
			return nil, err
		}
		_, _ = g.run("symbolic-ref", "refs/remotes/"+remote+"/HEAD", "refs/remotes/"+remote+"/"+target)
	}
	if err := g.checkBranchName(target); err != nil {
		return nil, err
	}
	if err := g.fetch(remote, url); err != nil {
		return nil, err
	}
	onto := remote + "/" + target
	up, err := g.oid("refs/remotes/" + onto)
	if err != nil {
		return nil, gitErrf("%s has no branch %s", remote, target)
	}
	base, err := g.oid("HEAD")
	if err != nil {
		return nil, gitErrf("the branch %s has no commits", branch)
	}
	res := &GitResult{Onto: onto}
	mb, _ := g.run("merge-base", base, up)
	switch {
	case mb == up:
		res.Outcome, res.Message = "up_to_date", branch+" already contains "+onto
		return res, nil
	case mb == base:
		res.Outcome = "fast_forward"
	case strategy == "ff-only":
		return nil, gitErrf("%s and %s have diverged; ff-only refuses (sync with strategy rebase to rebase your commits onto it)", branch, onto)
	default:
		res.Outcome = "rebased"
	}
	ident := g.identEnv()
	snap, err := g.save(base, branch, ident)
	if err != nil {
		return nil, fmt.Errorf("saving uncommitted changes (nothing was changed): %w", err)
	}
	if snap != nil {
		res.Saved = snap.saved
		if err := g.clear(snap); err != nil {
			return res, g.restore(res, snap, base, ident, fmt.Errorf("clearing the work tree: %w", err))
		}
	}
	if res.Outcome == "fast_forward" {
		if _, err := g.run("merge", "--ff-only", "-q", "--no-stat", "--no-verify", up); err != nil {
			return res, g.restore(res, snap, base, ident, err)
		}
	} else {
		o, err := g.exec(nil, ident, "rebase", "-q", "--no-autosquash", "--no-autostash", "--no-update-refs", up)
		if err != nil || o.code != 0 {
			conflicts, _ := g.run("diff", "--name-only", "--diff-filter=U", "-z")
			_, _ = g.exec(nil, nil, "rebase", "--abort")
			res.Outcome = "rebase_conflict"
			for _, f := range strings.Split(conflicts, "\x00") {
				if f != "" {
					res.Conflicts = append(res.Conflicts, f)
				}
			}
			res.Message = fmt.Sprintf("rebasing %s onto %s conflicted, so it was aborted: %s is as it was. Rebase by hand, or merge, resolving the conflicts.", branch, onto, branch)
			if o.err != "" && len(res.Conflicts) == 0 {
				res.Message += " git said: " + o.err
			}
			if snap != nil {
				if _, err := g.reapply(snap, base, ident); err != nil {
					return res, fmt.Errorf("%s; reapplying your uncommitted changes failed (%v): they are saved in %s and %s", res.Message, err, snap.saved.Commit, snap.saved.Patch)
				}
				res.Message += " Your uncommitted changes are back as they were."
			}
			return res, nil
		}
	}
	if snap != nil {
		files, err := g.reapply(snap, base, ident)
		if err != nil {
			return res, fmt.Errorf("%s is now on %s, but reapplying your uncommitted changes failed (%v): they are saved in commit %s and in %s", branch, onto, err, snap.saved.Commit, snap.saved.Patch)
		}
		if len(files) > 0 {
			res.Outcome, res.Conflicts = "conflict", files
			res.Message = fmt.Sprintf("%s is now on %s; your uncommitted changes conflicted with it in the files listed: they have conflict markers to resolve. Everything is unstaged. A copy of your changes is in commit %s (git stash list) and %s.", branch, onto, snap.saved.Commit, snap.saved.Patch)
			return res, nil
		}
		res.Message = fmt.Sprintf("%s is now on %s, with your uncommitted changes reapplied (a copy is in commit %s and %s).", branch, onto, snap.saved.Commit, snap.saved.Patch)
		return res, nil
	}
	res.Message = branch + " is now on " + onto
	return res, nil
}

// restore puts the snapshot back after a failure before HEAD moved
// (or after it moved back), and returns the error to report.
func (g *gitRunner) restore(res *GitResult, snap *gitSnapshot, base string, ident []string, cause error) error {
	if snap == nil {
		return cause
	}
	if _, err := g.run("reset", "--hard", "-q", "--no-recurse-submodules", base); err != nil {
		return fmt.Errorf("%v; restoring failed too (%v): your changes are saved in commit %s and in %s", cause, err, snap.saved.Commit, snap.saved.Patch)
	}
	if _, err := g.reapply(snap, base, ident); err != nil {
		return fmt.Errorf("%v; restoring failed too (%v): your changes are saved in commit %s and in %s", cause, err, snap.saved.Commit, snap.saved.Patch)
	}
	return fmt.Errorf("%v (nothing changed; your uncommitted changes are back, and a copy is in %s)", cause, snap.saved.Patch)
}

// --- commit ---

// repoPaths turns the agent's paths (relative to its cwd, or absolute)
// into paths relative to the work tree, refusing any outside it or in .git.
func (g *gitRunner) repoPaths(cwd string, ps []string) ([]string, error) {
	rcwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range ps {
		if strings.TrimSpace(p) == "" || strings.ContainsRune(p, 0) {
			return nil, gitErrf("paths: empty entry")
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(rcwd, p)
		}
		full = filepath.Clean(full)
		if !within(g.repo.top, full) {
			return nil, gitErrf("path %q is outside the repository %s", p, g.repo.top)
		}
		rel, _ := filepath.Rel(g.repo.top, full)
		if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); strings.EqualFold(first, ".git") {
			return nil, gitErrf("path %q is in .git", p)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

func (g *gitRunner) commit(a GitArgs, cwd string) (*GitResult, error) {
	if strings.TrimSpace(a.Message) == "" {
		return nil, gitErrf("commit needs a message")
	}
	if err := g.inProgress(); err != nil {
		return nil, err
	}
	paths, err := g.repoPaths(cwd, a.Paths)
	if err != nil {
		return nil, err
	}
	res := &GitResult{Outcome: "ok"}
	if b := a.NewBranch; b != "" {
		if err := g.checkBranchName(b); err != nil {
			return nil, err
		}
		if _, err := g.oid("refs/heads/" + b); err == nil {
			if cur, _ := g.currentBranch(); cur != b {
				if _, err := g.run("switch", "-q", "--no-recurse-submodules", b); err != nil {
					return nil, err
				}
			}
		} else if _, err := g.run("switch", "-q", "--no-recurse-submodules", "-c", b); err != nil {
			return nil, err
		}
	}
	branch, err := g.currentBranch()
	if err != nil {
		return nil, err
	}
	lit := []string{"GIT_LITERAL_PATHSPECS=1"}
	addArgs := []string{"add", "-A"}
	if len(paths) > 0 {
		addArgs = append(append(addArgs, "--"), paths...)
	}
	if _, err := g.runWith(nil, lit, addArgs...); err != nil {
		return nil, err
	}
	diffArgs := []string{"diff", "--cached", "--quiet", "--no-ext-diff"}
	if len(paths) > 0 {
		diffArgs = append(append(diffArgs, "--"), paths...)
	}
	if o, err := g.exec(nil, lit, diffArgs...); err != nil {
		return nil, err
	} else if o.code == 0 {
		return nil, gitErrf("nothing to commit")
	}
	ident := g.identEnv()
	cargs := []string{"commit", "-q", "--no-verify", "--no-edit", "-m", a.Message}
	if len(paths) > 0 {
		cargs = append(append(cargs, "--"), paths...)
	}
	if _, err := g.runWith(nil, append(ident, lit...), cargs...); err != nil {
		return nil, err
	}
	res.Commit, _ = g.oid("HEAD")
	res.Author = strings.TrimPrefix(ident[0], "GIT_AUTHOR_NAME=") + " <" + strings.TrimPrefix(ident[1], "GIT_AUTHOR_EMAIL=") + ">"
	res.Message = "committed on " + branch
	return res, nil
}

// --- push ---

// pushable refuses branches workspace_git never pushes or deletes.
func (g *gitRunner) pushable(branch, remote, url string) error {
	for _, p := range []string{"main", "master"} {
		if strings.EqualFold(branch, p) {
			return gitErrf("refusing to push %s: workspace_git never pushes main or master; commit on a branch of your own (commit with new_branch) and push that", branch)
		}
	}
	if h, err := g.run("symbolic-ref", "-q", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && strings.EqualFold(strings.TrimPrefix(h, remote+"/"), branch) {
		return gitErrf("refusing to push %s: it is %s's default branch", branch, remote)
	}
	head, err := g.remoteHead(url)
	if err != nil {
		return fmt.Errorf("cannot tell %s's default branch, so not pushing: %w", remote, err)
	}
	if strings.EqualFold(head, branch) {
		return gitErrf("refusing to push %s: it is %s's default branch", branch, remote)
	}
	if pre := g.n.cfg.Git.PushBranchPrefixes; len(pre) > 0 {
		_, sname, _ := strings.Cut(g.session, "/")
		var allowed []string
		for _, p := range pre {
			p = strings.ReplaceAll(strings.ReplaceAll(p, "<host>", g.host), "<session>", sname)
			allowed = append(allowed, p)
			if strings.HasPrefix(branch, p) {
				return nil
			}
		}
		return gitErrf("refusing to push %s: on this host workspace_git pushes only branches starting with %s", branch, strings.Join(allowed, " or "))
	}
	return nil
}

func (g *gitRunner) pushRemote(branch string) (string, error) {
	remote, _ := g.run("config", "--get", "branch."+branch+".remote")
	if remote == "" || remote == "." {
		remote = "origin"
	}
	return remote, g.checkRemoteName(remote)
}

func (g *gitRunner) push(a GitArgs) (*GitResult, error) {
	if g.canPush != "" {
		return nil, gitErrf("%s", g.canPush)
	}
	branch := a.Branch
	if branch == "" {
		var err error
		if branch, err = g.currentBranch(); err != nil {
			return nil, err
		}
	}
	if err := g.checkBranchName(branch); err != nil {
		return nil, err
	}
	oid, err := g.oid("refs/heads/" + branch)
	if err != nil {
		return nil, gitErrf("no local branch %s", branch)
	}
	remote, err := g.pushRemote(branch)
	if err != nil {
		return nil, err
	}
	url, err := g.remoteURL(remote, true)
	if err != nil {
		return nil, err
	}
	if err := g.pushable(branch, remote, url); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	if _, err := g.run("push", "--porcelain", "--no-verify", "--recurse-submodules=no", "--quiet", url, ref+":"+ref); err != nil {
		return nil, err
	}
	res := &GitResult{Outcome: "ok", Pushed: remote + "/" + branch, Commit: oid,
		Message: fmt.Sprintf("pushed %s to %s (%s)", branch, remote, url)}
	if err := g.n.gitPushed(g.repo.common, url, branch, true); err != nil {
		res.Warnings = append(res.Warnings, "recording the push: "+err.Error())
	}
	if _, err := g.run("update-ref", "-m", "workspace_git push", "refs/remotes/"+remote+"/"+branch, oid); err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	if r, _ := g.upstream(branch); r == "" {
		_, err1 := g.run("config", "branch."+branch+".remote", remote)
		_, err2 := g.run("config", "branch."+branch+".merge", ref)
		if err := errors.Join(err1, err2); err != nil {
			res.Warnings = append(res.Warnings, "setting the upstream: "+err.Error())
		}
	}
	return res, nil
}

// --- branch_delete ---

func (g *gitRunner) branchDelete(a GitArgs) (*GitResult, error) {
	b := a.Branch
	if err := g.checkBranchName(b); err != nil {
		return nil, err
	}
	if cur, _ := g.currentBranch(); cur == b {
		return nil, gitErrf("%s is checked out; switch to another branch first", b)
	}
	res := &GitResult{Outcome: "ok"}
	var did []string
	if a.DeleteRemote {
		if g.canPush != "" {
			return nil, gitErrf("%s", g.canPush)
		}
		remote, err := g.pushRemote(b)
		if err != nil {
			return nil, err
		}
		url, err := g.remoteURL(remote, true)
		if err != nil {
			return nil, err
		}
		if !g.n.gitWasPushed(g.repo.common, url, b) {
			return nil, gitErrf("refusing to delete %s on %s: workspace_git did not push it", b, remote)
		}
		if err := g.pushable(b, remote, url); err != nil {
			return nil, err
		}
		if _, err := g.run("push", "--porcelain", "--no-verify", "--quiet", url, ":refs/heads/"+b); err != nil {
			return nil, err
		}
		_ = g.n.gitPushed(g.repo.common, url, b, false)
		_, _ = g.run("update-ref", "-d", "refs/remotes/"+remote+"/"+b)
		did = append(did, "deleted "+b+" on "+remote)
	}
	if _, err := g.oid("refs/heads/" + b); err == nil {
		old, _ := g.oid("refs/heads/" + b)
		if _, err := g.run("branch", "-D", "-q", b); err != nil {
			return nil, err
		}
		did = append(did, "deleted the local branch "+b+" (was "+old+")")
	} else if !a.DeleteRemote {
		return nil, gitErrf("no local branch %s", b)
	}
	res.Message = strings.Join(did, "; ")
	return res, nil
}

// --- what this tool pushed ---

func (n *Node) gitPushedPath() string { return filepath.Join(n.dir, "git", "pushed.json") }

func (n *Node) loadPushed() map[string][]string {
	m := map[string][]string{}
	if b, err := os.ReadFile(n.gitPushedPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func pushedKey(common, url string) string { return filepath.ToSlash(common) + " " + url }

func (n *Node) gitWasPushed(common, url, branch string) bool {
	n.gitMu.Lock()
	defer n.gitMu.Unlock()
	return slices.Contains(n.loadPushed()[pushedKey(common, url)], branch)
}

func (n *Node) gitPushed(common, url, branch string, add bool) error {
	n.gitMu.Lock()
	defer n.gitMu.Unlock()
	m := n.loadPushed()
	k := pushedKey(common, url)
	l := slices.DeleteFunc(m[k], func(s string) bool { return s == branch })
	if add {
		l = append(l, branch)
		if len(l) > 1000 {
			l = l[len(l)-1000:]
		}
	}
	if len(l) == 0 {
		delete(m, k)
	} else {
		m[k] = l
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(n.gitPushedPath()), 0o700); err != nil {
		return err
	}
	return paths.WriteFile(n.gitPushedPath(), b, 0o600)
}
