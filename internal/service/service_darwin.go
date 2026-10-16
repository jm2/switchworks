// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/paths"
)

const managerName = "launchd"

func unitName(role string) string { return "dev.switchworks." + role }

func command(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func plistPath(role string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", unitName(role)+".plist"), nil
}

func domain() string            { return "gui/" + strconv.Itoa(os.Getuid()) }
func target(role string) string { return domain() + "/" + unitName(role) }

func query(role string) (state, error) {
	var st state
	path, err := plistPath(role)
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err == nil {
		st.installed, st.enabled = true, true
	}
	out, err := command("launchctl", "print", target(role)).Output()
	if err != nil {
		return st, nil // not loaded
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "state = running" {
			st.active = true
			break
		}
	}
	return st, nil
}

// launchPATH is the PATH the agent gets. LaunchAgents start with a minimal
// PATH, so harnesses installed with Homebrew, npm or a version manager would
// not resolve. Take the login shell's PATH (where Homebrew's shellenv and
// version managers usually set it up), then this process's, and make sure
// the usual locations are present.
func launchPATH() string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		for _, d := range filepath.SplitList(p) {
			// Only absolute directories: a relative entry such as "."
			// would make lookups depend on the agent's working directory.
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
	add("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	return strings.Join(parts, ":")
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func plistText(role, exe, path, logFile string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by ` + "`swk service install " + role + "`" + `; ` + "`swk service uninstall " + role + "`" + ` removes it.
     A LaunchAgent runs as you, in your login session. Re-run the install to
     pick up a new PATH. -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlText(unitName(role)) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlText(exe) + `</string>
    <string>` + role + `</string>
    <string>run</string>
    <string>--log-file</string>
    <string>` + xmlText(logFile) + `</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + xmlText(path) + `</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <!-- Restart after a crash, not after a clean stop. -->
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
  </dict>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Interactive</string>
  <!-- The log itself is --log-file (rotated at 10 MiB); this only catches
       anything printed before it opens. -->
  <key>StandardOutPath</key><string>` + xmlText(logFile+".launchd") + `</string>
  <key>StandardErrorPath</key><string>` + xmlText(logFile+".launchd") + `</string>
</dict>
</plist>
`
}

func install(role, exe string, noStart bool, out io.Writer) error {
	path, err := plistPath(role)
	if err != nil {
		return err
	}
	logFile := filepath.Join(LogDir(role), role+".log")
	if err := os.MkdirAll(LogDir(role), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Replace a loaded agent so the new definition takes effect.
	_ = command("launchctl", "bootout", target(role)).Run()
	if err := paths.WriteFile(path, []byte(plistText(role, exe, launchPATH(), logFile)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s (runs %s; log %s)\n", path, exe, logFile)
	if noStart {
		fmt.Fprintln(out, "It starts at your next login.")
		return nil
	}
	if _, err := run("launchctl", "bootstrap", domain(), path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Started %s\n", unitName(role))
	return nil
}

func uninstall(role string, out io.Writer) error {
	path, err := plistPath(role)
	if err != nil {
		return err
	}
	if command("launchctl", "bootout", target(role)).Run() == nil {
		fmt.Fprintf(out, "Stopped %s\n", unitName(role))
	}
	if err := os.Remove(path); err == nil {
		fmt.Fprintf(out, "Removed %s\n", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loaded(role string) bool { return command("launchctl", "print", target(role)).Run() == nil }

func start(role string) error {
	if loaded(role) {
		_, err := run("launchctl", "kickstart", target(role))
		return err
	}
	path, err := plistPath(role)
	if err != nil {
		return err
	}
	_, err = run("launchctl", "bootstrap", domain(), path)
	return err
}

// stop unloads the agent; launchd loads it again at the next login.
func stop(role string) error {
	if !loaded(role) {
		return nil
	}
	_, err := run("launchctl", "bootout", target(role))
	return err
}

func restart(role string) error {
	if loaded(role) {
		_, err := run("launchctl", "kickstart", "-k", target(role))
		return err
	}
	return start(role)
}
