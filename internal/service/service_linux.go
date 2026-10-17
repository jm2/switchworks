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

// pathDropIn is the drop-in Install writes beside the unit, packaged or
// not, to give it the PATH of your login shell (see loginPATH): a user
// unit's own is only /usr/local/bin:/usr/bin.
func pathDropIn(role string) (string, error) {
	unit, err := userUnitPath(role)
	if err != nil {
		return "", err
	}
	return filepath.Join(unit+".d", "swk-path.conf"), nil
}

// dropInText renders the drop-in that sets PATH.
func dropInText(path string) ([]byte, error) {
	if strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, fmt.Errorf("PATH contains control characters; not writing it into a unit")
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return []byte(generatedMarker +
		"# Your login shell's PATH, so harnesses in ~/.local/bin or installed with\n" +
		"# npm or a version manager resolve. Re-run the install to pick up a new one.\n" +
		"[Service]\nEnvironment=\"PATH=" + r.Replace(path) + "\"\n"), nil
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
	q, err := systemdQuote(exe)
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(b, []byte("@SWK@"), []byte(q)), nil
}

// systemdQuote quotes a path for ExecStart. It refuses control characters:
// a newline would end ExecStart and start a directive of the path's
// choosing, and quoting cannot carry one.
func systemdQuote(s string) (string, error) {
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("the path %q contains control characters; not writing a unit for it", s)
	}
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\;$") {
		return s, nil
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$")
	return `"` + r.Replace(s) + `"`, nil
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
	}
	dropIn, err := pathDropIn(role)
	if err != nil {
		return err
	}
	text, err := dropInText(loginPATH("/usr/local/bin:/usr/bin:/bin"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		return err
	}
	if err := paths.WriteFile(dropIn, text, 0o644); err != nil {
		return err
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		return err
	}
	// A running unit keeps its old PATH until it restarts.
	if st, err := query(role); err == nil && st.active && !noStart {
		if _, err := systemctl("restart", unit); err != nil {
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
	dropIn, err := pathDropIn(role)
	if err != nil {
		return err
	}
	removed := false
	for _, p := range []string{path, dropIn} {
		if b, err := os.ReadFile(p); err == nil && bytes.HasPrefix(b, []byte(generatedMarker)) {
			if err := os.Remove(p); err != nil {
				return err
			}
			fmt.Fprintf(out, "Removed %s\n", p)
			removed = true
		}
	}
	// The drop-in directory goes too, if nothing else is in it.
	_ = os.Remove(filepath.Dir(dropIn))
	if removed {
		_, _ = systemctl("daemon-reload")
	}
	return nil
}

func start(role string) error   { _, err := systemctl("start", unitName(role)); return err }
func stop(role string) error    { _, err := systemctl("stop", unitName(role)); return err }
func restart(role string) error { _, err := systemctl("restart", unitName(role)); return err }
