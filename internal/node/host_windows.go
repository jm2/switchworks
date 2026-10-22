// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os"
	"os/exec"
	"path/filepath"
)

// hostCodexConfig adds the Windows sandbox default (windowsSandboxDefault)
// to Codex's overrides. config.toml is read only to see whether it sets
// one; nothing from it is logged.
func hostCodexConfig(cfg Config, over map[string]string, logf func(string, ...any)) {
	home := cfg.Codex.Home
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	var toml []byte
	if home != "" {
		toml, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	}
	if windowsSandboxDefault(over, cfg.Codex.Args, toml) {
		logf(`codex: no Windows sandbox mode in config.toml or node.json; passing -c windows.sandbox="unelevated"`)
	}
}

// checkCodexHost warns when Codex's sandbox cannot start pwsh.
func checkCodexHost(logf func(string, ...any)) {
	p, err := exec.LookPath("pwsh")
	if err != nil {
		return // Codex uses Windows PowerShell instead
	}
	size := int64(-1)
	if fi, err := os.Lstat(p); err == nil {
		size = fi.Size()
	}
	if isAppExecAlias(p, size, os.Getenv("LOCALAPPDATA")) {
		logf("%s", pwshWarning(p))
	}
}
