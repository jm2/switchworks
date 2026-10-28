// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/switchworks/internal/adapter/codexad"
	"github.com/jm2/switchworks/internal/codex"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/version"
)

// Session caches: inside Codex's sandbox ~/.cache is not writable, so
// ccache and gh fail. Each Codex session gets a directory of its own,
// <cache root>/<session id> (0700; the root defaults to
// <user cache dir>/switchworks/session-cache, beside the received files
// and outside ~/.config/swk, which profiles deny), made writable for that
// thread only and named by XDG_CACHE_HOME and CCACHE_DIR in its commands'
// environment (codexad.Cache). Another session cannot write it, so one
// agent cannot poison another's cache. It is kept across node restarts
// and removed when the session stops, fails to start or is forgotten; at
// start the node removes those of sessions it no longer has.

// cacheEnv are the variables pointed at a session's cache directory.
var cacheEnv = []string{"XDG_CACHE_HOME", "CCACHE_DIR"}

// cacheRoot is the directory session caches go in, or "" for none.
func (n *Node) cacheRoot() string {
	if n.cfg.SessionCache.Disabled {
		return ""
	}
	if d := n.cfg.SessionCache.Dir; d != "" {
		d, err := expandHome(d)
		if err != nil {
			return ""
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return ""
		}
		return abs
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, version.Slug, "session-cache")
}

// cacheDirFor makes (or keeps) the cache directory of session id and
// returns it, resolved through symlinks as sandboxes want their paths
// (macOS's seatbelt matches real paths), or "" when there is none.
func (n *Node) cacheDirFor(id string) string {
	root := n.cacheRoot()
	if root == "" || !ulid.Valid(id) {
		return ""
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		n.logf("session cache: %v", err)
		return ""
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		n.logf("session cache: %v", err)
		return ""
	}
	dir := filepath.Join(real, id)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		n.logf("session cache: %v", err)
		return ""
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		n.logf("session cache: %s is not a directory", dir)
		return ""
	}
	return dir
}

// sessionCache is the cache directory a session of harnessName gets:
// only Codex sessions have one (codexad makes it writable per thread).
func (n *Node) sessionCache(harnessName, id string) string {
	if harnessName != "codex" && harnessName != "" {
		return ""
	}
	return n.cacheDirFor(id)
}

// dropCache removes session id's cache directory.
func (n *Node) dropCache(id string) {
	if root := n.cacheRoot(); root != "" && ulid.Valid(id) {
		_ = os.RemoveAll(filepath.Join(root, id))
	}
}

// sweepCaches removes the cache directories of sessions other than keep.
// Only entries named like a session id are touched, so a cache root
// shared with something else loses nothing else.
func (n *Node) sweepCaches(keep map[string]bool) {
	root := n.cacheRoot()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ulid.Valid(e.Name()) && !keep[e.Name()] {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// codexCache works out how Codex threads get their cache directory from
// the node's overrides (over, with -c ones in codex.args) and config.toml
// (nil when absent): which variables the owner leaves for it, the legacy
// writable roots the per-thread override must keep, and whether the
// permission profile's filesystem table can take an entry. logf reports
// what stops the directory being made writable; config.toml's contents
// are never logged.
func codexCache(cfg Config, over map[string]string, configTOML []byte, logf func(string, ...any)) codexad.Cache {
	all := map[string]string{}
	for k, v := range over {
		all[k] = v
	}
	for k, v := range codex.ConfigArgs(cfg.Codex.Args) {
		all[k] = v
	}
	var c codexad.Cache
next:
	for _, v := range cacheEnv {
		for k := range all {
			if k == "shell_environment_policy" || k == "shell_environment_policy.set" ||
				strings.EqualFold(k, "shell_environment_policy.set."+v) {
				continue next
			}
		}
		for k := range cfg.Codex.SessionEnv.Set {
			if strings.EqualFold(k, v) {
				continue next
			}
		}
		if codex.ConfigSetsEnv(configTOML, v) {
			continue
		}
		c.Env = append(c.Env, v)
	}

	// Legacy workspace-write: the override replaces writable_roots, so it
	// carries the ones the thread would have had.
	const rootsKey = "sandbox_workspace_write.writable_roots"
	if _, whole := all["sandbox_workspace_write"]; whole {
		logf("session cache: codex.config or codex.args sets sandbox_workspace_write as a whole; " +
			"set sandbox_workspace_write.writable_roots instead, or the cache directories stay read-only under the legacy sandbox")
	} else if v, ok := all[rootsKey]; ok {
		roots, err := codex.ParseStringArray(v)
		if err != nil {
			logf("session cache: cannot read %s from codex.config or codex.args (%v); the cache directories stay read-only under the legacy sandbox", rootsKey, err)
		} else {
			c.Roots, c.RootsKnown = roots, true
		}
	} else if roots, _, ok := codex.ConfigWritableRoots(configTOML); ok {
		c.Roots, c.RootsKnown = roots, true
	} else {
		logf("session cache: cannot read %s in config.toml (write it as an array of strings in [sandbox_workspace_write]); "+
			"the cache directories stay read-only under the legacy sandbox", rootsKey)
	}

	// A permission profile: a per-thread filesystem table merges into
	// config.toml's, but would replace one set with -c.
	if p := cfg.Codex.Permissions; p != "" {
		c.ProfileOK = true
		for k := range all {
			if k == "permissions" || k == "permissions."+p || k == "permissions."+p+".filesystem" ||
				strings.HasPrefix(k, "permissions."+p+".filesystem.") {
				c.ProfileOK = false
				logf("session cache: codex.config or codex.args sets %s; the cache directories stay read-only under profile %s "+
					"unless the profile lets them be written", k, p)
				break
			}
		}
	}
	return c
}
