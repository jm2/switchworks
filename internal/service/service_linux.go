// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package service

import (
	"bytes"
	"embed"
	"fmt"
	"github.com/jm2/switchworks/internal/paths"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const managerName = "systemd"

//go:embed units/*.service
var units embed.FS

// generatedMarker starts every unit file Install writes, so Uninstall removes
// only its own files and never a packaged or hand-written unit.
const generatedMarker = "# Written by `swk service install`; `swk service uninstall` removes it.\n"

func unitName(role string) string { return "swk-" + role + ".service" }

func command(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func systemctl(args ...string) (string, error) {
	return run("systemctl", append([]string{"--user"}, args...)...)
}

// userUnitPath is where Install writes a unit when no package provides one.
func userUnitPath(role string) (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName(role)), nil
}

func query(role string) (state, error) {
	var st state
	out, err := systemctl("show", "--property=LoadState,UnitFileState,ActiveState", unitName(role))
	if err != nil {
		return st, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	switch props["UnitFileState"] {
	case "enabled", "enabled-runtime", "linked", "linked-runtime":
		st.enabled = true
	}
	switch props["ActiveState"] {
	case "active", "activating", "reloading":
		st.active = true
	}
	// "Installed" means set up for this user: enabled, or running under
	// systemd. A packaged unit nobody enabled does not count.
	st.installed = st.enabled || st.active
	return st, nil
}

// unitText renders a unit for exe.
func unitText(role, exe string) ([]byte, error) {
	b, err := units.ReadFile("units/" + unitName(role))
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(b, []byte("@SWK@"), []byte(systemdQuote(exe))), nil
}

// systemdQuote quotes a path for ExecStart.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\;$") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// packagedUnit reports whether some unit file other than one Install wrote
// provides role (a distribution package, or one written by hand).
func packagedUnit(role string) bool {
	out, err := systemctl("show", "--property=FragmentPath", unitName(role))
	if err != nil {
		return false
	}
	_, path, _ := strings.Cut(strings.TrimSpace(out), "=")
	if path == "" {
		return false
	}
	own, _ := userUnitPath(role)
	if path == own {
		b, err := os.ReadFile(path)
		return err == nil && !bytes.HasPrefix(b, []byte(generatedMarker))
	}
	return true
}

func install(role, exe string, noStart bool, out io.Writer) error {
	unit := unitName(role)
	if packagedUnit(role) {
		fmt.Fprintf(out, "Using the installed unit %s\n", unit)
	} else {
		path, err := userUnitPath(role)
		if err != nil {
			return err
		}
		text, err := unitText(role, exe)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := paths.WriteFile(path, append([]byte(generatedMarker), text...), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s (runs %s)\n", path, exe)
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	args := []string{"enable", unit}
	if !noStart {
		args = []string{"enable", "--now", unit}
	}
	if _, err := systemctl(args...); err != nil {
		return err
	}
	if noStart {
		fmt.Fprintf(out, "Enabled %s; it starts at your next login (or: systemctl --user start %s)\n", unit, unit)
	} else {
		fmt.Fprintf(out, "Enabled and started %s (logs: journalctl --user -u %s)\n", unit, unit)
	}
	if u, err := user.Current(); err == nil {
		if v, err := run("loginctl", "show-user", u.Uid, "--property=Linger", "--value"); err == nil && v == "no" {
			fmt.Fprintf(out, "To keep it running while you are logged out: loginctl enable-linger %s\n", u.Username)
		}
	}
	return nil
}

func uninstall(role string, out io.Writer) error {
	unit := unitName(role)
	if st, err := query(role); err == nil && (st.enabled || st.active) {
		if _, err := systemctl("disable", "--now", unit); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped and disabled %s\n", unit)
	}
	path, err := userUnitPath(role)
	if err != nil {
		return err
	}
	if b, err := os.ReadFile(path); err == nil && bytes.HasPrefix(b, []byte(generatedMarker)) {
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %s\n", path)
		_, _ = systemctl("daemon-reload")
	}
	return nil
}

func start(role string) error   { _, err := systemctl("start", unitName(role)); return err }
func stop(role string) error    { _, err := systemctl("stop", unitName(role)); return err }
func restart(role string) error { _, err := systemctl("restart", unitName(role)); return err }
