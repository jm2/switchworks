// SPDX-License-Identifier: GPL-3.0-or-later

// Package service registers the node and coordinator with the user's own
// service manager, and reports how they are doing:
//
//   - Linux: a systemd user unit (swk-node.service, swk-coordinator.service).
//     A distribution package ships them in /usr/lib/systemd/user; otherwise
//     Install writes one into ~/.config/systemd/user.
//   - macOS: a LaunchAgent in ~/Library/LaunchAgents (dev.switchworks.node).
//   - Windows: a Task Scheduler task that starts at logon in the user's
//     session, with least privilege ("Switchworks node (<user>, <SID>)").
//
// These run as the user who installs them and need no root or administrator
// rights. The node must run as you, so sessions get your sign-ins,
// repositories and toolchains. The coordinator can instead be a system
// service under an account of its own (see system.go), which root or an
// administrator sets up; it still never runs as root or SYSTEM.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/version"
)

// Roles a service can have.
const (
	Node        = "node"
	Coordinator = "coordinator"
)

// Roles lists every role, node first.
var Roles = []string{Node, Coordinator}

// ErrNotInstalled is returned for a role that has no service registered.
var ErrNotInstalled = errors.New("service is not installed")

// Info describes one role on this machine.
type Info struct {
	Role    string `json:"role"`
	Manager string `json:"manager"` // systemd, launchd, task-scheduler, scm, or none
	Unit    string `json:"unit"`    // unit name, launchd label, task or service name
	// System: the coordinator's system service, under its own account,
	// rather than one that runs as this user.
	System bool `json:"system,omitempty"`
	// Configured: node.json or coordinator.json exists (enrolled / initialized).
	Configured bool `json:"configured"`
	Installed  bool `json:"installed"` // registered with the service manager
	Enabled    bool `json:"enabled"`   // starts at login
	// Running: the service manager says it runs, or a fresh status file
	// shows a process started some other way (such as in a terminal).
	Running bool              `json:"running"`
	Status  *runstatus.Status `json:"status,omitempty"` // when fresh
	Dir     string            `json:"dir"`
	LogDir  string            `json:"log_dir"`
	Error   string            `json:"error,omitempty"` // the service manager could not be asked
}

// Overview is what `swk service status --json` prints and the tray apps
// read.
type Overview struct {
	Version     string `json:"version"`
	Node        Info   `json:"node"`
	Coordinator Info   `json:"coordinator"`
	// WebUI is the best known web UI address: the local coordinator's, then
	// the CLI config's, then the coordinator this node is enrolled with.
	WebUI     string `json:"web_ui,omitempty"`
	ConfigDir string `json:"config_dir"`
}

// state is what a platform's service manager reports.
type state struct {
	installed, enabled, active bool
}

// Dir returns a role's directory.
func Dir(role string) string {
	if role == Coordinator {
		return paths.CoordinatorDir()
	}
	return paths.NodeDir()
}

// LogDir is where a role's logs go when the service manager does not keep
// them (the node's harness.log is there on every platform).
func LogDir(role string) string { return filepath.Join(Dir(role), "logs") }

func configFile(role string) string {
	if role == Coordinator {
		return "coordinator.json"
	}
	return "node.json"
}

func checkRole(role string) error {
	if role != Node && role != Coordinator {
		return fmt.Errorf("unknown role %q (want node or coordinator)", role)
	}
	return nil
}

// Query reports on one role.
func Query(role string) Info {
	in := Info{Role: role, Manager: managerName, Unit: unitName(role), Dir: Dir(role), LogDir: LogDir(role)}
	if _, err := os.Stat(filepath.Join(in.Dir, configFile(role))); err == nil {
		in.Configured = true
	}
	st, err := query(role)
	if err != nil {
		in.Error = err.Error()
	}
	in.Installed, in.Enabled, in.Running = st.installed, st.enabled, st.active
	if s, err := runstatus.Read(in.Dir); err == nil && s.Fresh(time.Now()) {
		in.Status = &s
		in.Running = true
	}
	// With no coordinator of this user's own, report the system one.
	if role == Coordinator && !in.Installed && !in.Running {
		if sys, ok := querySystemInfo(); ok {
			return sys
		}
	}
	return in
}

// Status reports on both roles.
func Status() Overview {
	o := Overview{Version: version.Version, Node: Query(Node), Coordinator: Query(Coordinator), ConfigDir: paths.Base()}
	o.WebUI = webUI(o)
	return o
}

// safeURL keeps only plain http and https addresses: the trays open this
// address, and the files it comes from should never make them open another
// scheme (file:, or an app's custom one).
func safeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return s
}

// webUI returns the first usable web UI address among the candidates.
func webUI(o Overview) string {
	for _, c := range webUICandidates(o) {
		if u := safeURL(c); u != "" {
			return u
		}
	}
	return ""
}

func webUICandidates(o Overview) []string {
	var out []string
	if s := o.Coordinator.Status; s != nil && s.WebUI != "" {
		out = append(out, s.WebUI)
	}
	var cc struct {
		PublicUI  string `json:"public_ui"`
		PublicAPI string `json:"public_api"`
	}
	if readJSON(filepath.Join(paths.CoordinatorDir(), "coordinator.json"), &cc) == nil {
		if cc.PublicUI != "" {
			out = append(out, strings.TrimSuffix(cc.PublicUI, "/")+"/")
		}
		if cc.PublicAPI != "" {
			out = append(out, "https://"+cc.PublicAPI+"/")
		}
	}
	// Only the address is read from cli.json, never the token.
	var cli struct {
		API string `json:"api"`
	}
	if readJSON(paths.CLIConfig(), &cli) == nil && cli.API != "" {
		out = append(out, strings.TrimSuffix(cli.API, "/")+"/")
	}
	if s := o.Node.Status; s != nil && s.WebUI != "" {
		out = append(out, s.WebUI)
	}
	var nc struct {
		CoordinatorAPI string `json:"coordinator_api"`
	}
	if readJSON(filepath.Join(paths.NodeDir(), "node.json"), &nc) == nil && nc.CoordinatorAPI != "" {
		out = append(out, "https://"+nc.CoordinatorAPI+"/")
	}
	return out
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Install registers role with the service manager, to start at login and
// now (unless noStart). exe is the swk binary the service runs; empty means
// this process's own executable.
func Install(role, exe string, noStart bool, out io.Writer) error {
	if err := checkRole(role); err != nil {
		return err
	}
	exe, err := resolveExe(exe)
	if err != nil {
		return err
	}
	return install(role, exe, noStart, out)
}

// Uninstall stops role and removes its registration. Configuration and data
// are kept.
func Uninstall(role string, out io.Writer) error {
	if err := checkRole(role); err != nil {
		return err
	}
	return uninstall(role, out)
}

// Start starts an installed service.
func Start(role string) error {
	if err := checkRole(role); err != nil {
		return err
	}
	if st, err := query(role); err == nil && !st.installed {
		return fmt.Errorf("%s: %w (run `%s service install %s`)", role, ErrNotInstalled, version.Bin, role)
	}
	return start(role)
}

// Stop stops a service; it starts again at the next login.
func Stop(role string) error {
	if err := checkRole(role); err != nil {
		return err
	}
	if st, err := query(role); err == nil && !st.installed {
		return fmt.Errorf("%s: %w", role, ErrNotInstalled)
	}
	return stop(role)
}

// Restart restarts a service, starting it if it was stopped.
func Restart(role string) error {
	if err := checkRole(role); err != nil {
		return err
	}
	if st, err := query(role); err == nil && !st.installed {
		return fmt.Errorf("%s: %w (run `%s service install %s`)", role, ErrNotInstalled, version.Bin, role)
	}
	return restart(role)
}

// Executable returns the swk binary to register: this process's own when it
// is swk, otherwise the swk beside it (as for the tray apps), otherwise the
// one on PATH.
func Executable() (string, error) {
	self, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(self); err == nil {
			self = r
		}
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(self)), ".exe")
		if base == version.Bin || base == version.Bin+"w" {
			return self, nil
		}
		sib := filepath.Join(filepath.Dir(self), version.Bin+exeSuffix())
		if fi, err := os.Stat(sib); err == nil && !fi.IsDir() {
			return sib, nil
		}
	}
	p, err := exec.LookPath(version.Bin)
	if err != nil {
		return "", fmt.Errorf("cannot find the %s binary: %w", version.Bin, err)
	}
	return filepath.Abs(p)
}

// checkExecutable refuses binaries the service manager could not rely on:
// on macOS one inside a mounted disk image or an App Translocation copy
// (gone after the next eject or reboot), and anywhere a path that service
// definitions cannot carry safely.
func checkExecutable(exe string) error {
	if strings.ContainsFunc(exe, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%q: control characters in the path", exe)
	}
	if runtime.GOOS == "darwin" && (strings.HasPrefix(exe, "/Volumes/") || strings.Contains(exe, "/AppTranslocation/")) {
		return fmt.Errorf("%s runs from a disk image or a temporary copy; move Switchworks.app to Applications first", exe)
	}
	if runtime.GOOS == "windows" && strings.Contains(exe, "%") {
		return fmt.Errorf("%q: Task Scheduler would expand the %% in this path; install somewhere without one", exe)
	}
	return nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// run runs a service-manager command and folds its output into the error.
func run(name string, args ...string) (string, error) {
	cmd := command(name, args...)
	b, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(b))
	if err != nil {
		label := strings.TrimSuffix(filepath.Base(name), ".exe")
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				label += " " + a
				break
			}
		}
		if out != "" {
			return out, fmt.Errorf("%s: %s", label, out)
		}
		return out, fmt.Errorf("%s: %w", label, err)
	}
	return out, nil
}

// waitStopped waits for a role's status file to go away or go stale.
func waitStopped(role string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		s, err := runstatus.Read(Dir(role))
		if err != nil || !s.Fresh(time.Now()) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
