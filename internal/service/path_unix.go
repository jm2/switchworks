// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin || linux

package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// loginPATH is the PATH a service gets. Service managers start it with a
// minimal PATH, so harnesses in ~/.local/bin (where codex, goose and agy
// install themselves), or installed with Homebrew, npm or a version manager,
// would not resolve. Take the login shell's PATH (where shell profiles and
// version managers usually set it up), then this process's, then
// ~/.local/bin and fallback.
func loginPATH(fallback string) string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		for _, d := range filepath.SplitList(p) {
			// Only absolute directories: a relative entry such as "."
			// would make lookups depend on the service's working directory.
			if d != "" && filepath.IsAbs(d) && !seen[d] {
				seen[d] = true
				parts = append(parts, d)
			}
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" && filepath.IsAbs(sh) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// A marker separates PATH from anything the profile prints.
		const mark = "__SWK_PATH__="
		out, err := exec.CommandContext(ctx, sh, "-l", "-c", `printf '\n`+mark+`%s\n' "$PATH"`).Output()
		cancel()
		if i := strings.LastIndex(string(out), mark); err == nil && i >= 0 {
			add(strings.TrimSpace(string(out[i+len(mark):])))
		}
	}
	add(os.Getenv("PATH"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".local", "bin"))
	}
	add(fallback)
	return strings.Join(parts, ":")
}
