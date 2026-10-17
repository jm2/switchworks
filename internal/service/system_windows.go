// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/version"
	"github.com/jm2/switchworks/internal/winsys"
)

const (
	systemManagerName = "scm"
	systemUnitName    = "swk-coordinator"
	// SystemAccount is the system coordinator's virtual account: Windows
	// manages it, it has no password, and only this service runs as it.
	SystemAccount = `NT SERVICE\` + systemUnitName
	firewallRule  = "Switchworks coordinator"
)

var errSystemUnsupported = errors.New("unsupported") // never: Windows has a system service

func systemHome() string {
	pd := os.Getenv("ProgramData")
	if pd == "" || !filepath.IsAbs(pd) {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "Switchworks")
}

// systemBinDir holds the swk.exe the service runs. Switchworks installs per
// user, into a folder that user (and their agents) can write; the service
// runs a copy under Program Files, which only administrators can change.
func systemBinDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" || !filepath.IsAbs(pf) {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Switchworks Coordinator")
}

func system32(exe string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", exe)
}

func sc(args ...string) (string, error) { return run(system32("sc.exe"), args...) }

var (
	advapi32                 = winsys.DLL("advapi32.dll")
	procOpenSCManagerW       = advapi32.NewProc("OpenSCManagerW")
	procOpenServiceW         = advapi32.NewProc("OpenServiceW")
	procQueryServiceStatus   = advapi32.NewProc("QueryServiceStatus")
	procQueryServiceConfigW  = advapi32.NewProc("QueryServiceConfigW")
	procCloseServiceHandle   = advapi32.NewProc("CloseServiceHandle")
	procCheckTokenMembershp  = advapi32.NewProc("CheckTokenMembership")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	procLocalFree            = winsys.DLL("kernel32.dll").NewProc("LocalFree")
)

// owner returns the SID of the owner of path.
func owner(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	const seFileObject, ownerSecurityInformation = 1, 1
	var sid *syscall.SID
	var sd uintptr
	r, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject, ownerSecurityInformation,
		uintptr(unsafe.Pointer(&sid)), 0, 0, 0, uintptr(unsafe.Pointer(&sd)))
	if r != 0 {
		return "", syscall.Errno(r)
	}
	defer procLocalFree.Call(sd)
	return sid.String()
}

const (
	scManagerConnect     = 0x0001
	serviceQueryConfig   = 0x0001
	serviceQueryStatus   = 0x0004
	serviceAutoStart     = 2
	errServiceNotExist   = syscall.Errno(1060)
	serviceStopped       = 1
	serviceStartPending  = 2
	serviceStopPending   = 3
	serviceRunning       = 4
	serviceContinuePend  = 5
	servicePausePending  = 6
	servicePaused        = 7
	serviceStatusDWORDs  = 7
	waitForStateInterval = 250 * time.Millisecond
)

// serviceState asks the service control manager for the service's state
// (0 when it does not exist).
func serviceState() (uint32, error) {
	s, _, err := serviceInfo()
	return s, err
}

// serviceInfo returns the service's state (0 when it does not exist) and
// whether it starts at boot.
func serviceInfo() (uint32, bool, error) {
	m, _, err := procOpenSCManagerW.Call(0, 0, scManagerConnect)
	if m == 0 {
		return 0, false, err
	}
	defer procCloseServiceHandle.Call(m)
	name, _ := syscall.UTF16PtrFromString(systemUnitName)
	h, _, err := procOpenServiceW.Call(m, uintptr(unsafe.Pointer(name)), serviceQueryStatus|serviceQueryConfig)
	if h == 0 {
		if errors.Is(err, errServiceNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	defer procCloseServiceHandle.Call(h)
	var st [serviceStatusDWORDs]uint32
	if r, _, err := procQueryServiceStatus.Call(h, uintptr(unsafe.Pointer(&st[0]))); r == 0 {
		return 0, false, err
	}
	// QUERY_SERVICE_CONFIGW, whose second DWORD is dwStartType; 8 KiB is
	// the most it can take.
	buf := make([]uint32, 2048)
	var need uint32
	auto := false
	if r, _, _ := procQueryServiceConfigW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*4), uintptr(unsafe.Pointer(&need))); r != 0 {
		auto = buf[1] == serviceAutoStart
	}
	return st[1], auto, nil // dwCurrentState
}

func querySystem() (state, error) {
	var st state
	s, auto, err := serviceInfo()
	if err != nil || s == 0 {
		return st, err
	}
	st.installed, st.enabled = true, auto
	st.active = s == serviceRunning || s == serviceStartPending || s == serviceContinuePend
	return st, nil
}

// isAdmin reports whether this process runs with the Administrators group
// enabled: elevated, not just an administrator's filtered token.
func isAdmin() bool {
	// S-1-5-32-544
	sid, err := syscall.StringToSid("S-1-5-32-544")
	if err != nil {
		return false
	}
	var member int32
	r, _, _ := procCheckTokenMembershp.Call(0, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&member)))
	return r != 0 && member != 0
}

func requireAdmin(what string) error {
	if !isAdmin() {
		return fmt.Errorf("%s needs administrator rights: run it from an elevated prompt", what)
	}
	return nil
}

// copyBinary puts exe in systemBinDir as swk.exe. The service must be
// stopped: Windows will not replace a running program.
func copyBinary(exe string, out io.Writer) (string, error) {
	dst := filepath.Join(systemBinDir(), version.Bin+".exe")
	if strings.EqualFold(filepath.Clean(exe), dst) {
		return dst, nil
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	// Program Files lets only administrators (and SYSTEM and TrustedInstaller)
	// write, and the new folder inherits that.
	if err := os.MkdirAll(systemBinDir(), 0o755); err != nil {
		return "", err
	}
	if err := paths.WriteFile(dst, b, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "Copied %s to %s (run this install again after upgrading Switchworks)\n", exe, dst)
	return dst, nil
}

// quoteArg quotes one argument of the service's command line.
func quoteArg(s string) string { return syscall.EscapeArg(s) }

// serviceCommand is the command line the service control manager runs.
func serviceCommand(bin string) string {
	dir := systemDir()
	return strings.Join([]string{
		quoteArg(bin), "coordinator", "run", "--windows-service",
		"--dir", quoteArg(dir),
		"--log-file", quoteArg(filepath.Join(dir, "logs", "coordinator.log")),
	}, " ")
}

// secureSystemHome creates %ProgramData%\Switchworks, owned by
// Administrators, and replaces its permissions with full control for SYSTEM
// and Administrators and modify for the service's own account, inherited by
// everything in it. ProgramData lets any user create a folder, so one that
// an administrator or SYSTEM does not own is refused, as is a link; what is
// inside is made to inherit the new permissions.
func secureSystemHome() error {
	home := systemHome()
	if fi, err := os.Lstat(home); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is a link or a file, not a folder; remove it and run this again", home)
		}
		o, err := owner(home)
		if err != nil {
			return err
		}
		if o != "S-1-5-32-544" && o != "S-1-5-18" {
			return fmt.Errorf("%s exists and belongs to %s, not to administrators; someone other than an administrator may have made it. Check it, remove it and run this again", home, o)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	icacls := system32("icacls.exe")
	if _, err := run(icacls, home, "/setowner", "*S-1-5-32-544", "/T", "/C", "/L", "/Q"); err != nil {
		return err
	}
	if _, err := run(icacls, home, "/inheritance:r",
		"/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", SystemAccount+":(OI)(CI)M", "/L", "/Q"); err != nil {
		return err
	}
	// Anything already inside takes the folder's permissions only.
	if entries, err := os.ReadDir(home); err == nil && len(entries) > 0 {
		if _, err := run(icacls, filepath.Join(home, "*"), "/reset", "/T", "/C", "/L", "/Q"); err != nil {
			return err
		}
	}
	return nil
}

// waitState waits for the service to reach want.
func waitState(want uint32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		s, err := serviceState()
		if err == nil && (s == want || (want == serviceStopped && s == 0)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(waitForStateInterval)
	}
}

func installSystem(exe string, noStart bool, out io.Writer) error {
	if err := requireAdmin("swk service install --system"); err != nil {
		return err
	}
	s, err := serviceState()
	if err != nil {
		return err
	}
	exists := s != 0
	if exists && s != serviceStopped {
		if _, err := sc("stop", systemUnitName); err != nil {
			return err
		}
		if !waitState(serviceStopped, 30*time.Second) {
			return errors.New("the coordinator service did not stop within 30 seconds")
		}
	}
	bin, err := copyBinary(exe, out)
	if err != nil {
		return err
	}
	verb := "create"
	if exists {
		verb = "config"
	}
	// It starts at boot only once it is set up (swk service start
	// --system turns that on), so a reboot before init does not fail.
	start := "demand"
	if systemConfigured(false) {
		start = "auto"
	}
	if _, err := sc(verb, systemUnitName,
		"binPath=", serviceCommand(bin),
		"start=", start,
		"obj=", SystemAccount,
		"DisplayName=", "Switchworks coordinator"); err != nil {
		return err
	}
	// Its own service SID (what the virtual account is), only the privilege
	// every process has, and a restart after a crash or a failed exit
	// (failureflag: the coordinator reports its own failures as stopped,
	// with an exit code).
	for _, args := range [][]string{
		{"description", systemUnitName, "Switchworks coordinator: the fleet's API, web UI and node port. Runs as its own account; set up with `swk --system coordinator init`."},
		{"sidtype", systemUnitName, "unrestricted"},
		{"privs", systemUnitName, "SeChangeNotifyPrivilege"},
		{"failure", systemUnitName, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/30000"},
		{"failureflag", systemUnitName, "1"},
	} {
		if _, err := sc(args...); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Registered the service %s (runs %s as %s)\n", systemUnitName, bin, SystemAccount)
	if err := secureSystemHome(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Made %s private to %s, SYSTEM and administrators\n", systemHome(), SystemAccount)
	// Windows Firewall asks nobody about a service: allow it in, on private
	// and domain networks only, like the prompt's default.
	netsh := system32("netsh.exe")
	_, _ = run(netsh, "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	if _, err := run(netsh, "advfirewall", "firewall", "add", "rule", "name="+firewallRule,
		"dir=in", "action=allow", "program="+bin, "service="+systemUnitName,
		"protocol=TCP", "profile=private,domain", "enable=yes"); err != nil {
		fmt.Fprintf(out, "Could not add a firewall rule (%v); allow TCP 7480 and 7481 in yourself\n", err)
	} else {
		fmt.Fprintf(out, "Allowed it through Windows Firewall on private and domain networks\n")
	}
	configured := systemConfigured(false)
	switch {
	case !configured:
		fmt.Fprintf(out, `It will start at boot once it is set up and started. Next, from this elevated prompt:
  %s --system coordinator init --host <names nodes dial>
  %s service start --system coordinator
`, version.Bin, version.Bin)
	case noStart:
		fmt.Fprintln(out, "It starts at boot.")
	default:
		if err := startSystem(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Started it (log: %s)\n", filepath.Join(systemDir(), "logs", "coordinator.log"))
	}
	return nil
}

func uninstallSystem(out io.Writer) error {
	if err := requireAdmin("swk service uninstall --system"); err != nil {
		return err
	}
	s, err := serviceState()
	if err != nil {
		return err
	}
	if s != 0 {
		if s != serviceStopped {
			_ = stopSystem()
		}
		if _, err := sc("delete", systemUnitName); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed the service %s\n", systemUnitName)
	}
	_, _ = run(system32("netsh.exe"), "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	bin := filepath.Join(systemBinDir(), version.Bin+".exe")
	if err := os.Remove(bin); err == nil {
		_ = os.Remove(systemBinDir())
		fmt.Fprintf(out, "Removed %s\n", bin)
	}
	fmt.Fprintf(out, "Kept %s (the CA and the fleet's state); delete it yourself if you no longer need it.\n", systemHome())
	return nil
}

func startSystem() error {
	// From now on it starts at boot too.
	if _, err := sc("config", systemUnitName, "start=", "auto"); err != nil {
		return err
	}
	if _, err := sc("start", systemUnitName); err != nil {
		return err
	}
	if !waitState(serviceRunning, 30*time.Second) {
		return errors.New("the coordinator service did not start within 30 seconds (see its log)")
	}
	return nil
}

func stopSystem() error {
	if s, err := serviceState(); err == nil && (s == serviceStopped || s == 0) {
		return nil
	}
	if _, err := sc("stop", systemUnitName); err != nil {
		return err
	}
	if !waitState(serviceStopped, 30*time.Second) {
		return errors.New("the coordinator service did not stop within 30 seconds")
	}
	return nil
}

func restartSystem() error {
	if err := stopSystem(); err != nil {
		return err
	}
	return startSystem()
}

// enterSystem points this process at the system coordinator's files. It
// needs an elevated prompt: the folder lets only administrators in, besides
// the service, and what an administrator writes there inherits that.
func enterSystem(args []string, stderr io.Writer) (int, bool) {
	if err := requireAdmin(version.Bin + " --system"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1, true
	}
	if _, err := os.Stat(systemHome()); err != nil {
		fmt.Fprintf(stderr, "%v: run `%s service install --system coordinator` first\n", err, version.Bin)
		return 1, true
	}
	os.Setenv("SWK_HOME", systemHome())
	os.Setenv("SWK_SYSTEM", "1")
	os.Unsetenv("SWK_CLI_CONFIG")
	return 0, false
}
