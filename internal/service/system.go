// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/version"
)

// The coordinator can also run as a system service under an account of its
// own, so agents running as you on the same machine cannot read its CA key,
// admin token or fleet state:
//
//   - Linux: the swk-coordinator.service system unit, as the unprivileged
//     switchworks user, with its files in /var/lib/switchworks.
//   - Windows: the swk-coordinator service, as its virtual account
//     NT SERVICE\swk-coordinator, with its files in
//     %ProgramData%\Switchworks (only that account, SYSTEM and
//     administrators may open them).
//
// Setting it up needs root or an elevated prompt; the coordinator itself
// never runs with either. Nodes are never system services: sessions need
// your sign-ins, repositories and toolchains.

// errSystemRole refuses a system service for anything but the coordinator.
var errSystemRole = errors.New("only the coordinator runs as a system service; the node runs as you (swk service install node)")

// SystemHome is the system coordinator's base directory (its SWK_HOME), or
// "" where there is no system service.
func SystemHome() string { return systemHome() }

// systemDir is the system coordinator's directory.
func systemDir() string { return filepath.Join(systemHome(), "coordinator") }

// systemConfigured reports whether the system coordinator is initialized.
// Its directory is private to its account, so when this user cannot look,
// an installed service is taken to be set up.
func systemConfigured(installed bool) bool {
	_, err := os.Stat(filepath.Join(systemDir(), "coordinator.json"))
	if err == nil {
		return true
	}
	return installed && errors.Is(err, os.ErrPermission)
}

// querySystemInfo reports on the system coordinator, if one is installed.
func querySystemInfo() (Info, bool) {
	if systemHome() == "" {
		return Info{}, false
	}
	st, err := querySystem()
	if err != nil || !st.installed {
		return Info{}, false
	}
	in := Info{
		Role: Coordinator, Manager: systemManagerName, Unit: systemUnitName, System: true,
		Dir: systemDir(), LogDir: filepath.Join(systemDir(), "logs"),
		Installed: st.installed, Enabled: st.enabled, Running: st.active,
	}
	in.Configured = systemConfigured(true)
	if s, err := runstatus.Read(in.Dir); err == nil && s.Fresh(time.Now()) {
		in.Status = &s
		in.Running = true
	}
	return in, true
}

// InstallSystem registers the coordinator as a system service, to start at
// boot, and starts it now if it is initialized (unless noStart). It creates
// the service account and its directory if needed. exe is the swk binary
// to run; one that a non-administrator could replace is copied somewhere
// only administrators can write first.
func InstallSystem(role, exe string, noStart bool, out io.Writer) error {
	if role != Coordinator {
		return errSystemRole
	}
	exe, err := resolveExe(exe)
	if err != nil {
		return err
	}
	return installSystem(exe, noStart, out)
}

// UninstallSystem stops the system coordinator and removes its service.
// Its account and directory, with the CA and the fleet's state, are kept.
func UninstallSystem(role string, out io.Writer) error {
	if role != Coordinator {
		return errSystemRole
	}
	return uninstallSystem(out)
}

func checkSystemInstalled(role string) error {
	if role != Coordinator {
		return errSystemRole
	}
	if st, err := querySystem(); err == nil && !st.installed {
		return fmt.Errorf("the system coordinator: %w (run `%s service install --system coordinator`)", ErrNotInstalled, version.Bin)
	}
	return nil
}

// errSystemNotSetUp refuses to start a system coordinator that has no
// configuration yet.
var errSystemNotSetUp = fmt.Errorf("the system coordinator is not set up yet: run `%s --system coordinator init --host <names nodes dial>` first", version.Bin)

// checkSystemSetUp refuses to start the system coordinator before init. A
// directory this user cannot look into is left to the service manager.
func checkSystemSetUp() error {
	if _, err := os.Stat(filepath.Join(systemDir(), "coordinator.json")); errors.Is(err, os.ErrNotExist) {
		return errSystemNotSetUp
	}
	return nil
}

// StartSystem starts the system coordinator (and on Windows has it start
// at boot from then on).
func StartSystem(role string) error {
	if err := checkSystemInstalled(role); err != nil {
		return err
	}
	if err := checkSystemSetUp(); err != nil {
		return err
	}
	return startSystem()
}

// StopSystem stops the system coordinator; it starts again at boot.
func StopSystem(role string) error {
	if err := checkSystemInstalled(role); err != nil {
		return err
	}
	return stopSystem()
}

// RestartSystem restarts the system coordinator, starting it if stopped.
func RestartSystem(role string) error {
	if err := checkSystemInstalled(role); err != nil {
		return err
	}
	if err := checkSystemSetUp(); err != nil {
		return err
	}
	return restartSystem()
}

// systemCommands are the commands `swk --system` runs against the system
// coordinator: its own, and the CLI's (with its admin cli.json). Never the
// node, the MCP server or the dev tools: those would run agents as the
// coordinator's account, beside its CA key.
func systemCommandAllowed(cmd string) bool {
	switch cmd {
	case "node", "mcp", "dev", "service", "help", "-h", "--help", "version", "--version", "-v", "--system":
		return false
	}
	return cmd != ""
}

const systemUsage = `usage: swk --system <command> [args]

Runs a coordinator or CLI command against the system coordinator (set up
with ` + "`swk service install --system coordinator`" + `), as its account and with
its files: for example

  sudo swk --system coordinator init --host coord.lan,10.0.0.5
  sudo swk --system coordinator enroll-token devbox | swk node enroll -
  sudo swk --system token create "$USER" --scope read --cli-config

On Windows, run it from an elevated prompt instead of with sudo.`

// EnterSystem prepares `swk --system <args>`. When done is true the command
// has already run (on Linux, as the service account, in a child process)
// and code is its exit status; otherwise this process now uses the system
// coordinator's files and should run args itself.
func EnterSystem(args []string, stderr io.Writer) (code int, done bool) {
	if len(args) == 0 || !systemCommandAllowed(args[0]) {
		fmt.Fprintln(stderr, systemUsage)
		return 2, true
	}
	if systemHome() == "" {
		fmt.Fprintf(stderr, "%s --system: %v\n", version.Bin, errSystemUnsupported)
		return 1, true
	}
	return enterSystem(args, stderr)
}

// resolveExe finds the swk binary a service should run: exe, or this
// process's own when empty, as an absolute path with symlinks resolved.
func resolveExe(exe string) (string, error) {
	if exe == "" {
		var err error
		if exe, err = Executable(); err != nil {
			return "", err
		}
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	// Check (and register) where it really is, not a symlink to it.
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if err := checkExecutable(exe); err != nil {
		return "", err
	}
	return exe, nil
}
