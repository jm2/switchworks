// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
)

// gitFixture is a bare "remote" with main, a clone of it (the session's
// working directory) and a second clone standing in for other agents.
type gitFixture struct {
	t      *testing.T
	remote string
	work   string
	other  string
	n      *Node
	s      *session
}

// rawGit runs git as a test would by hand (not through workspace_git).
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "user.name=Tester",
		"-c", "user.email=tester@example.com", "-c", "init.defaultBranch=main", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// The user's own git config stays out of it.
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	writeFile(t, global, "[core]\n\tautocrlf = false\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &gitFixture{t: t, remote: filepath.Join(base, "remote.git"), work: filepath.Join(base, "work"), other: filepath.Join(base, "other")}
	rawGit(t, base, "init", "-q", "--bare", "-b", "main", f.remote)
	rawGit(t, base, "clone", "-q", f.remote, f.other)
	writeFile(t, filepath.Join(f.other, "a.txt"), "one\ntwo\nthree\n")
	writeFile(t, filepath.Join(f.other, ".gitignore"), "*.log\n")
	rawGit(t, f.other, "add", "-A")
	rawGit(t, f.other, "commit", "-q", "-m", "first")
	rawGit(t, f.other, "push", "-q", "origin", "main")
	rawGit(t, base, "clone", "-q", f.remote, f.work)
	rawGit(t, f.work, "config", "user.name", "Work Agent")
	rawGit(t, f.work, "config", "user.email", "work@example.com")
	f.n = &Node{dir: filepath.Join(base, "node"), cfg: Config{Host: "h"}, gitAllowLocal: true}
	f.s = &session{n: f.n, rec: record{ID: "s1", Path: "h/s1", Spec: adapter.StartSpec{Cwd: f.work}}}
	return f
}

// upstreamCommit commits a change to a.txt in the other clone and pushes it.
func (f *gitFixture) upstreamCommit(content string) {
	writeFile(f.t, filepath.Join(f.other, "a.txt"), content)
	rawGit(f.t, f.other, "commit", "-q", "-am", "upstream change")
	rawGit(f.t, f.other, "push", "-q", "origin", "main")
}

func (f *gitFixture) do(args map[string]any) (*GitResult, error) {
	f.t.Helper()
	b, _ := json.Marshal(args)
	return f.n.handleGit(context.Background(), f.s, b)
}

func (f *gitFixture) ok(args map[string]any) *GitResult {
	f.t.Helper()
	res, err := f.do(args)
	if err != nil {
		f.t.Fatalf("%v: %v", args, err)
	}
	return res
}

func TestGitSyncClean(t *testing.T) {
	f := newGitFixture(t)
	res := f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "up_to_date" {
		t.Fatalf("first sync: %+v", res)
	}
	f.upstreamCommit("one\ntwo\nthree\nfour\n")
	res = f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "fast_forward" || res.Saved != nil || res.Onto != "origin/main" {
		t.Fatalf("sync: %+v", res)
	}
	if got := readFile(t, filepath.Join(f.work, "a.txt")); got != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if !res.Status.Clean || res.Status.Branch != "main" || res.Status.Behind != 0 || res.Status.Upstream != "origin/main" {
		t.Fatalf("status: %+v", res.Status)
	}
}

func TestGitSyncCarriesChanges(t *testing.T) {
	f := newGitFixture(t)
	f.upstreamCommit("zero\none\ntwo\nthree\n")
	// A tracked edit (staged), an untracked file, an ignored one.
	writeFile(t, filepath.Join(f.work, "a.txt"), "one\ntwo\nthree\nmine\n")
	rawGit(t, f.work, "add", "a.txt")
	writeFile(t, filepath.Join(f.work, "dir", "new.txt"), "new file\n")
	writeFile(t, filepath.Join(f.work, "debug.log"), "ignored\n")
	res := f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "fast_forward" || res.Saved == nil || len(res.Conflicts) > 0 {
		t.Fatalf("sync: %+v", res)
	}
	if got := readFile(t, filepath.Join(f.work, "a.txt")); got != "zero\none\ntwo\nthree\nmine\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(f.work, "dir", "new.txt")); got != "new file\n" {
		t.Fatalf("new.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(f.work, "debug.log")); got != "ignored\n" {
		t.Fatalf("debug.log = %q", got)
	}
	// Still untracked, and the staged edit staged again.
	if st := rawGit(t, f.work, "status", "--porcelain"); !strings.Contains(st, "?? dir/") || !strings.Contains(st, "M  a.txt") {
		t.Fatalf("status:\n%s", st)
	}
	if _, err := os.Stat(res.Saved.Patch); err != nil {
		t.Fatalf("saved patch: %v", err)
	}
	if !strings.Contains(rawGit(t, f.work, "stash", "list"), "workspace_git sync") {
		t.Fatal("no stash entry")
	}
	if !strings.Contains(readFile(t, res.Saved.Patch), "new file") {
		t.Fatal("patch lacks the untracked file")
	}
}

func TestGitSyncConflictKeepsChanges(t *testing.T) {
	f := newGitFixture(t)
	f.upstreamCommit("one\nTWO upstream\nthree\n")
	writeFile(t, filepath.Join(f.work, "a.txt"), "one\nTWO mine\nthree\n")
	writeFile(t, filepath.Join(f.work, "notes.txt"), "keep me\n")
	res := f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "conflict" || !slices.Equal(res.Conflicts, []string{"a.txt"}) || res.Saved == nil {
		t.Fatalf("sync: %+v", res)
	}
	got := readFile(t, filepath.Join(f.work, "a.txt"))
	if !strings.Contains(got, "<<<<<<<") || !strings.Contains(got, "TWO mine") || !strings.Contains(got, "TWO upstream") {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(f.work, "notes.txt")); got != "keep me\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	// HEAD moved, and nothing is left in progress.
	if rawGit(t, f.work, "rev-parse", "HEAD") != rawGit(t, f.work, "rev-parse", "origin/main") {
		t.Fatal("HEAD not on origin/main")
	}
	if strings.Contains(rawGit(t, f.work, "status"), "progress") {
		t.Fatal(rawGit(t, f.work, "status"))
	}
	if !strings.Contains(readFile(t, res.Saved.Patch), "TWO mine") {
		t.Fatal("patch lacks the change")
	}
}

func TestGitSyncFFOnlyRefusesDiverged(t *testing.T) {
	f := newGitFixture(t)
	f.upstreamCommit("one\ntwo\nthree\nupstream\n")
	writeFile(t, filepath.Join(f.work, "b.txt"), "local\n")
	rawGit(t, f.work, "add", "b.txt")
	rawGit(t, f.work, "commit", "-q", "-m", "local")
	writeFile(t, filepath.Join(f.work, "wip.txt"), "wip\n")
	head := rawGit(t, f.work, "rev-parse", "HEAD")
	_, err := f.do(map[string]any{"op": "sync", "strategy": "ff-only"})
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("ff-only: %v", err)
	}
	if rawGit(t, f.work, "rev-parse", "HEAD") != head || readFile(t, filepath.Join(f.work, "wip.txt")) != "wip\n" {
		t.Fatal("ff-only refusal changed the work tree")
	}
}

func TestGitSyncRebasesLocalCommits(t *testing.T) {
	f := newGitFixture(t)
	f.upstreamCommit("one\ntwo\nthree\nupstream\n")
	writeFile(t, filepath.Join(f.work, "b.txt"), "local\n")
	rawGit(t, f.work, "add", "b.txt")
	rawGit(t, f.work, "commit", "-q", "-m", "local work")
	writeFile(t, filepath.Join(f.work, "wip.txt"), "wip\n")
	res := f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "rebased" || res.Status.Ahead != 1 || res.Status.Behind != 0 {
		t.Fatalf("sync: %+v %+v", res, res.Status)
	}
	if rawGit(t, f.work, "rev-parse", "HEAD~1") != rawGit(t, f.work, "rev-parse", "origin/main") {
		t.Fatal("local commit not on top of origin/main")
	}
	if rawGit(t, f.work, "log", "-1", "--format=%s %an") != "local work Tester" {
		t.Fatal(rawGit(t, f.work, "log", "-1", "--format=%s %an"))
	}
	if readFile(t, filepath.Join(f.work, "wip.txt")) != "wip\n" {
		t.Fatal("wip lost")
	}

	// A rebase that conflicts is aborted, with nothing changed.
	f.upstreamCommit("one\ntwo\nthree\nupstream again\n")
	writeFile(t, filepath.Join(f.work, "a.txt"), "one\ntwo\nthree\nlocal edit\n")
	rawGit(t, f.work, "commit", "-q", "-am", "conflicting")
	head := rawGit(t, f.work, "rev-parse", "HEAD")
	res = f.ok(map[string]any{"op": "sync"})
	if res.Outcome != "rebase_conflict" || !slices.Contains(res.Conflicts, "a.txt") {
		t.Fatalf("conflicting rebase: %+v", res)
	}
	if rawGit(t, f.work, "rev-parse", "HEAD") != head || readFile(t, filepath.Join(f.work, "wip.txt")) != "wip\n" {
		t.Fatal("aborted rebase changed things")
	}
	if _, err := os.Stat(filepath.Join(f.work, ".git", "rebase-merge")); err == nil {
		t.Fatal("rebase left in progress")
	}
}

func TestGitCommitAndPush(t *testing.T) {
	f := newGitFixture(t)
	writeFile(t, filepath.Join(f.work, "feature.txt"), "feature\n")
	writeFile(t, filepath.Join(f.work, "other.txt"), "not this one\n")
	res := f.ok(map[string]any{"op": "commit", "message": "Add feature", "paths": []string{"feature.txt"}, "new_branch": "swk/h/feature"})
	if res.Commit == "" || res.Status.Branch != "swk/h/feature" || res.Author != "Work Agent <work@example.com>" {
		t.Fatalf("commit: %+v", res)
	}
	if !slices.Contains(res.Status.Changed, "?? other.txt") {
		t.Fatalf("other.txt committed too: %+v", res.Status)
	}
	res = f.ok(map[string]any{"op": "push"})
	if res.Pushed != "origin/swk/h/feature" {
		t.Fatalf("push: %+v", res)
	}
	if got := rawGit(t, f.remote, "rev-parse", "refs/heads/swk/h/feature"); got != res.Commit {
		t.Fatalf("remote has %s, want %s", got, res.Commit)
	}
	if res.Status.Upstream != "origin/swk/h/feature" || res.Status.Ahead != 0 {
		t.Fatalf("upstream not set: %+v", res.Status)
	}
	// Again, after another commit: a plain fast-forward push.
	writeFile(t, filepath.Join(f.work, "feature.txt"), "feature 2\n")
	f.ok(map[string]any{"op": "commit", "message": "More"})
	res = f.ok(map[string]any{"op": "push"})
	if rawGit(t, f.remote, "rev-parse", "refs/heads/swk/h/feature") != res.Commit {
		t.Fatal("second push missing")
	}

	// branch_delete: the remote branch goes only because this tool pushed it.
	rawGit(t, f.work, "switch", "-q", "main")
	rawGit(t, f.work, "branch", "-q", "never-pushed")
	if _, err := f.do(map[string]any{"op": "branch_delete", "branch": "never-pushed", "delete_remote": true}); err == nil || !strings.Contains(err.Error(), "did not push") {
		t.Fatalf("delete of an unpushed branch: %v", err)
	}
	f.ok(map[string]any{"op": "branch_delete", "branch": "swk/h/feature", "delete_remote": true})
	if out := rawGit(t, f.remote, "branch", "--list", "swk/h/feature"); out != "" {
		t.Fatalf("remote branch still there: %q", out)
	}
	if _, err := f.do(map[string]any{"op": "branch_delete", "branch": "main"}); err == nil {
		t.Fatal("deleted the checked-out branch")
	}
}

func TestGitCommitDefaultAuthor(t *testing.T) {
	f := newGitFixture(t)
	rawGit(t, f.work, "config", "user.name", "$(git config user.name)")
	rawGit(t, f.work, "config", "--unset", "user.email")
	writeFile(t, filepath.Join(f.work, "x.txt"), "x\n")
	res := f.ok(map[string]any{"op": "commit", "message": "x"})
	if res.Author != "h/s1 <noreply@switchworks.invalid>" {
		t.Fatalf("author %q", res.Author)
	}
	if got := rawGit(t, f.work, "log", "-1", "--format=%an <%ae>|%cn"); got != "h/s1 <noreply@switchworks.invalid>|h/s1" {
		t.Fatalf("log: %q", got)
	}
	if _, err := f.do(map[string]any{"op": "commit", "message": "again"}); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Fatalf("empty commit: %v", err)
	}
	if _, err := f.do(map[string]any{"op": "commit", "message": " "}); err == nil {
		t.Fatal("blank message accepted")
	}
}

func TestGitPushRefused(t *testing.T) {
	f := newGitFixture(t)
	writeFile(t, filepath.Join(f.work, "x.txt"), "x\n")
	f.ok(map[string]any{"op": "commit", "message": "x"})
	for _, b := range []string{"main", "master", "MAIN"} {
		if b != "main" {
			rawGit(t, f.work, "branch", "-q", "-f", b)
		}
		if _, err := f.do(map[string]any{"op": "push", "branch": b}); err == nil || !strings.Contains(err.Error(), "never pushes main") {
			t.Errorf("push %s: %v", b, err)
		}
	}
	// The remote's default branch, whatever its name.
	rawGit(t, f.remote, "branch", "-q", "trunk", "main")
	rawGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	rawGit(t, f.work, "branch", "-q", "trunk")
	if _, err := f.do(map[string]any{"op": "push", "branch": "trunk"}); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Errorf("push trunk: %v", err)
	}
	// A prefix rule.
	f.n.cfg.Git.PushBranchPrefixes = []string{"swk/<host>/"}
	rawGit(t, f.work, "branch", "-q", "feature")
	if _, err := f.do(map[string]any{"op": "push", "branch": "feature"}); err == nil || !strings.Contains(err.Error(), "swk/h/") {
		t.Errorf("push outside the prefix: %v", err)
	}
	rawGit(t, f.work, "branch", "-q", "swk/h/ok")
	f.ok(map[string]any{"op": "push", "branch": "swk/h/ok"})
	// No force: a push that is not a fast-forward fails.
	rawGit(t, f.work, "branch", "-q", "-f", "swk/h/ok", "main~1")
	if _, err := f.do(map[string]any{"op": "push", "branch": "swk/h/ok"}); err == nil {
		t.Error("non-fast-forward push went through")
	}
	for _, b := range []string{"-f", "@{-1}", "a:b", "+x"} {
		if _, err := f.do(map[string]any{"op": "push", "branch": b}); err == nil || !strings.Contains(err.Error(), "invalid branch") {
			t.Errorf("push %q: %v", b, err)
		}
	}
	// Without the test override, no credentials configured: refused.
	f.n.gitAllowLocal = false
	if _, err := f.do(map[string]any{"op": "push", "branch": "feature"}); err == nil || !strings.Contains(err.Error(), "gh_config_dir") {
		t.Errorf("push without credentials: %v", err)
	}
	// ...and a local remote is not spoken to at all.
	if _, err := f.do(map[string]any{"op": "sync"}); err == nil || !strings.Contains(err.Error(), "https only") {
		t.Errorf("sync from a local path: %v", err)
	}
	// A gh config dir without hosts.yml is reported.
	f.n.cfg.Git.GHConfigDir = t.TempDir()
	if _, err := f.do(map[string]any{"op": "push", "branch": "feature"}); err == nil || !strings.Contains(err.Error(), "hosts.yml") {
		t.Errorf("push with an empty gh dir: %v", err)
	}
}

// Hooks planted in .git/hooks or through core.hooksPath in the repository's
// config never run, and drivers in it make the repository refused.
func TestGitHooksNotRun(t *testing.T) {
	f := newGitFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := "#!/bin/sh\necho ran > '" + filepath.ToSlash(marker) + "'\n"
	evil := filepath.Join(t.TempDir(), "hooks")
	for _, dir := range []string{filepath.Join(f.work, ".git", "hooks"), evil} {
		for _, h := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push", "post-checkout", "post-merge", "pre-rebase", "post-rewrite", "reference-transaction"} {
			writeFile(t, filepath.Join(dir, h), hook)
			if err := os.Chmod(filepath.Join(dir, h), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if runtime.GOOS != "windows" {
		// The hook works when git runs it.
		writeFile(t, filepath.Join(f.work, "c.txt"), "c\n")
		rawGit(t, f.work, "add", "c.txt")
		rawGit(t, f.work, "commit", "-q", "-m", "control")
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("control: the hook did not run under plain git")
		}
		_ = os.Remove(marker)
	}
	rawGit(t, f.work, "config", "core.hooksPath", evil)
	rawGit(t, f.work, "config", "core.fsmonitor", "echo")
	f.upstreamCommit("one\ntwo\nthree\nup\n")
	writeFile(t, filepath.Join(f.work, "d.txt"), "d\n")
	f.ok(map[string]any{"op": "sync"})
	f.ok(map[string]any{"op": "commit", "message": "d", "new_branch": "feat"})
	f.ok(map[string]any{"op": "push"})
	f.ok(map[string]any{"op": "status"})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted hook ran")
	}

	rawGit(t, f.work, "config", "filter.x.clean", "evil")
	if _, err := f.do(map[string]any{"op": "status"}); err == nil || !strings.Contains(err.Error(), "filter.x.clean") {
		t.Fatalf("filter driver: %v", err)
	}
	rawGit(t, f.work, "config", "--unset", "filter.x.clean")
	rawGit(t, f.work, "config", "include.path", "../evil.cfg")
	if _, err := f.do(map[string]any{"op": "status"}); err == nil || !strings.Contains(err.Error(), "include.path") {
		t.Fatalf("include: %v", err)
	}
}

func TestGitPathsRefused(t *testing.T) {
	f := newGitFixture(t)
	if _, err := f.do(map[string]any{"op": "status", "path": f.other}); err == nil || !strings.Contains(err.Error(), "outside your working directory") {
		t.Fatalf("other repo: %v", err)
	}
	if _, err := f.do(map[string]any{"op": "status", "path": ".."}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("..: %v", err)
	}
	// A nested repository inside the working directory is not the session's.
	nested := filepath.Join(f.work, "nested")
	rawGit(t, f.work, "init", "-q", nested)
	if _, err := f.do(map[string]any{"op": "status", "path": "nested"}); err == nil || !strings.Contains(err.Error(), "not in your session's repository") {
		t.Fatalf("nested: %v", err)
	}
	f.ok(map[string]any{"op": "status", "path": "."})
	writeFile(t, filepath.Join(f.work, "y.txt"), "y\n")
	if _, err := f.do(map[string]any{"op": "commit", "message": "m", "paths": []string{"../other/a.txt"}}); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("commit outside: %v", err)
	}
	if _, err := f.do(map[string]any{"op": "commit", "message": "m", "paths": []string{".git/config"}}); err == nil || !strings.Contains(err.Error(), "in .git") {
		t.Fatalf("commit .git: %v", err)
	}
	if _, err := f.do(map[string]any{"op": "bogus"}); err == nil {
		t.Fatal("bogus op")
	}
	if _, err := f.do(map[string]any{"op": "status", "evil": true}); err == nil {
		t.Fatal("unknown field accepted")
	}

	// A session outside any repository.
	f.s.rec.Spec.Cwd = t.TempDir()
	if _, err := f.do(map[string]any{"op": "status"}); err == nil ||
		!strings.Contains(err.Error(), "not in a git repository") && !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("no repo: %v", err)
	}
}

// A .git file must be a normal linked worktree's; one pointing at a gitdir
// of the agent's making is refused.
func TestGitWorktreeChecks(t *testing.T) {
	f := newGitFixture(t)
	wt := filepath.Join(filepath.Dir(f.work), "wt")
	rawGit(t, f.work, "worktree", "add", "-q", "-b", "wtbranch", wt)
	f.s.rec.Spec.Cwd = wt
	res := f.ok(map[string]any{"op": "status"})
	if res.Status.Branch != "wtbranch" {
		t.Fatalf("worktree status: %+v", res.Status)
	}
	writeFile(t, filepath.Join(wt, "w.txt"), "w\n")
	f.ok(map[string]any{"op": "commit", "message": "in the worktree"})
	f.ok(map[string]any{"op": "push"})

	// A fabricated gitdir: a copy of the repository the agent could write.
	fake := filepath.Join(t.TempDir(), "fake")
	rawGit(t, filepath.Dir(fake), "init", "-q", fake)
	agent := filepath.Join(filepath.Dir(f.work), "agent")
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agent, ".git"), "gitdir: "+filepath.Join(fake, ".git")+"\n")
	f.s.rec.Spec.Cwd = agent
	if _, err := f.do(map[string]any{"op": "status"}); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("fabricated gitdir: %v", err)
	}
	// Another worktree's gitdir.
	writeFile(t, filepath.Join(agent, ".git"), "gitdir: "+filepath.Join(f.work, ".git", "worktrees", "wt")+"\n")
	if _, err := f.do(map[string]any{"op": "status"}); err == nil || !strings.Contains(err.Error(), "another worktree") {
		t.Fatalf("borrowed gitdir: %v", err)
	}
}

func TestHTTPSURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/jm2/switchworks.git": "https://github.com/jm2/switchworks.git",
		"git@github.com:jm2/switchworks.git":     "https://github.com/jm2/switchworks.git",
		"ssh://git@github.com/jm2/switchworks":   "https://github.com/jm2/switchworks",
		"ssh://git@github.com:22/jm2/x.git":      "https://github.com/jm2/x.git",
		"http://example.com/x":                   "",
		"git://example.com/x":                    "",
		"ext::sh -c evil":                        "",
		"/srv/x.git":                             "",
		"file:///srv/x.git":                      "",
		"C:foo":                                  "",
	} {
		got, err := httpsURL(in, false)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}
