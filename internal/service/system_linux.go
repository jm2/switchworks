// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package service

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/version"
)

const (
	systemManagerName = "systemd"
	systemUnitName    = "swk-coordinator.service"
	// SystemAccount is the system coordinator's user and group.
	SystemAccount = "switchworks"
	systemHomeDir = "/var/lib/switchworks"
	// systemUnitPath is where InstallSystem writes the unit when no package
	// provides one, and systemBinDir where it copies a swk that a
	// non-root user could replace.
	systemUnitPath = "/etc/systemd/system/" + systemUnitName
	systemBinDir   = "/usr/local/lib/switchworks"
)

//go:embed units/system/swk-coordinator.service
var systemUnit []byte

//go:embed units/switchworks.sysusers
var sysusersConf string

var errSystemUnsupported = errors.New("unsupported") // never: Linux has a system service

func systemHome() string { return systemHomeDir }

func systemctlSystem(args ...string) (string, error) { return run("systemctl", args...) }

func querySystem() (state, error) {
	var st state
	out, err := systemctlSystem("show", "--property=UnitFileState,ActiveState", systemUnitName)
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
	// As for user units: a packaged unit nobody enabled is not installed.
	st.installed = st.enabled || st.active
	return st, nil
}

// systemUnitText renders the system unit for exe.
func systemUnitText(exe string) ([]byte, error) {
	q, err := systemdQuote(exe)
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(systemUnit, []byte("@SWK@"), []byte(q)), nil
}

// packagedSystemUnit reports whether a unit file other than one
// InstallSystem wrote provides the system coordinator.
func packagedSystemUnit() bool {
	out, err := systemctlSystem("show", "--property=FragmentPath", systemUnitName)
	if err != nil {
		return false
	}
	_, path, _ := strings.Cut(strings.TrimSpace(out), "=")
	if path == "" {
		return false
	}
	if path == systemUnitPath {
		b, err := os.ReadFile(path)
		return err == nil && !bytes.HasPrefix(b, []byte(generatedMarker))
	}
	return true
}

func requireRoot(what string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s needs root: run it with sudo", what)
	}
	return nil
}

// systemAccount looks up the service account, creating it with
// systemd-sysusers (from the same line the packages ship) if create is set.
func systemAccount(create bool) (*user.User, error) {
	u, err := user.Lookup(SystemAccount)
	if err == nil || !create {
		if err != nil {
			return nil, fmt.Errorf("no %s account: run `sudo %s service install --system coordinator` first", SystemAccount, version.Bin)
		}
		return u, nil
	}
	var line string
	for _, l := range strings.Split(sysusersConf, "\n") {
		if strings.HasPrefix(l, "u ") {
			line = l
		}
	}
	if _, err := run("systemd-sysusers", "--inline", line); err != nil {
		return nil, fmt.Errorf("creating the %s account: %w", SystemAccount, err)
	}
	return user.Lookup(SystemAccount)
}

func ids(u *user.User) (uid, gid int, err error) {
	if uid, err = strconv.Atoi(u.Uid); err == nil {
		gid, err = strconv.Atoi(u.Gid)
	}
	return uid, gid, err
}

// ensureSystemHome creates /var/lib/switchworks for the account, private to
// it. systemd's StateDirectory= does the same when the service starts; this
// lets `swk --system coordinator init` run before that.
func ensureSystemHome(u *user.User) error {
	uid, gid, err := ids(u)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(systemHomeDir)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(systemHomeDir, 0o700); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", systemHomeDir)
	}
	if err := os.Lchown(systemHomeDir, uid, gid); err != nil {
		return err
	}
	return os.Chmod(systemHomeDir, 0o700)
}

// rootOnly reports whether path and every directory above it belong to root
// and are writable by no one else: a binary only root can replace.
func rootOnly(path string) bool {
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if p == "/" {
			return true
		}
	}
}

// systemBinary returns a swk for the system service to run: exe itself if
// only root can replace it (as for a packaged /usr/bin/swk), otherwise a
// copy in systemBinDir. A swk in your home directory, which you and your
// agents can write, would otherwise run as the coordinator's account.
func systemBinary(exe string, out io.Writer) (string, error) {
	if rootOnly(exe) {
		return exe, nil
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(systemBinDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(systemBinDir, version.Bin)
	if err := paths.WriteFile(dst, b, 0o755); err != nil {
		return "", err
	}
	if !rootOnly(dst) {
		return "", fmt.Errorf("%s can be changed by users other than root; not running it as a service", dst)
	}
	fmt.Fprintf(out, "Copied %s to %s (run this install again after upgrading swk)\n", exe, dst)
	return dst, nil
}

func installSystem(exe string, noStart bool, out io.Writer) error {
	if err := requireRoot("swk service install --system"); err != nil {
		return err
	}
	u, err := systemAccount(true)
	if err != nil {
		return err
	}
	if err := ensureSystemHome(u); err != nil {
		return err
	}
	if packagedSystemUnit() {
		fmt.Fprintf(out, "Using the installed unit %s\n", systemUnitName)
	} else {
		bin, err := systemBinary(exe, out)
		if err != nil {
			return err
		}
		text, err := systemUnitText(bin)
		if err != nil {
			return err
		}
		if err := paths.WriteFile(systemUnitPath, append([]byte(generatedMarker), text...), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s (runs %s as %s)\n", systemUnitPath, bin, SystemAccount)
		if _, err := systemctlSystem("daemon-reload"); err != nil {
			return err
		}
	}
	configured := systemConfigured(false)
	args := []string{"enable", systemUnitName}
	if configured && !noStart {
		args = []string{"enable", "--now", systemUnitName}
	}
	if _, err := systemctlSystem(args...); err != nil {
		return err
	}
	switch {
	case !configured:
		fmt.Fprintf(out, `Enabled %s; it starts at boot once it is set up. Next, set it up and start it:
  sudo %s --system coordinator init --host <names nodes dial>
  sudo %s service start --system coordinator
`, systemUnitName, version.Bin, version.Bin)
	case noStart:
		fmt.Fprintf(out, "Enabled %s; it starts at boot (or: sudo systemctl start %s)\n", systemUnitName, systemUnitName)
	default:
		fmt.Fprintf(out, "Enabled and started %s (logs: journalctl -u %s)\n", systemUnitName, systemUnitName)
	}
	return nil
}

func uninstallSystem(out io.Writer) error {
	if err := requireRoot("swk service uninstall --system"); err != nil {
		return err
	}
	if st, err := querySystem(); err == nil && (st.enabled || st.active) {
		if _, err := systemctlSystem("disable", "--now", systemUnitName); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped and disabled %s\n", systemUnitName)
	}
	if b, err := os.ReadFile(systemUnitPath); err == nil && bytes.HasPrefix(b, []byte(generatedMarker)) {
		if err := os.Remove(systemUnitPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %s\n", systemUnitPath)
		_, _ = systemctlSystem("daemon-reload")
	}
	if err := os.Remove(filepath.Join(systemBinDir, version.Bin)); err == nil {
		_ = os.Remove(systemBinDir)
		fmt.Fprintf(out, "Removed %s\n", filepath.Join(systemBinDir, version.Bin))
	}
	fmt.Fprintf(out, `Kept the %s account and %s (the CA and the fleet's state).
To delete them too: sudo rm -rf %s && sudo userdel %s
`, SystemAccount, systemHomeDir, systemHomeDir, SystemAccount)
	return nil
}

func startSystem() error   { _, err := systemctlSystem("start", systemUnitName); return err }
func stopSystem() error    { _, err := systemctlSystem("stop", systemUnitName); return err }
func restartSystem() error { _, err := systemctlSystem("restart", systemUnitName); return err }

// systemEnv is the environment a command run as the service account gets:
// the locale and terminal from the caller, nothing else of theirs.
func systemEnv(caller []string, u *user.User) []string {
	env := []string{
		"SWK_HOME=" + systemHomeDir,
		"SWK_SYSTEM=1",
		"HOME=" + systemHomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	for _, kv := range caller {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TERM" || k == "LANG" || k == "TZ" || k == "NO_COLOR" || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// enterSystem runs args as the service account. As root, it runs this
// command again in a child process with the account's user and group and
// no others; as the account itself (sudo -u switchworks), it just uses the
// system files.
func enterSystem(args []string, stderr io.Writer) (int, bool) {
	if cur, err := user.Current(); err == nil && cur.Username == SystemAccount {
		os.Setenv("SWK_HOME", systemHomeDir)
		os.Setenv("SWK_SYSTEM", "1")
		os.Unsetenv("SWK_CLI_CONFIG")
		return 0, false
	}
	if err := requireRoot(version.Bin + " --system"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1, true
	}
	u, err := systemAccount(false)
	var uid, gid int
	if err == nil {
		err = ensureSystemHome(u)
	}
	if err == nil {
		uid, gid, err = ids(u)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1, true
	}
	// The account must be able to run the binary: prefer the one the
	// service runs.
	exe := filepath.Join(systemBinDir, version.Bin)
	if _, err := os.Stat(exe); err != nil {
		if exe, err = os.Executable(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, true
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = systemHomeDir
	cmd.Env = systemEnv(os.Environ(), u)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), true
		}
		fmt.Fprintf(stderr, "running %s as %s: %v\n", exe, SystemAccount, err)
		return 1, true
	}
	return 0, true
}
