// SPDX-License-Identifier: GPL-3.0-or-later

// Package dbustest starts a private dbus-daemon for tests.
package dbustest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// StartBus runs a private session bus for the test and returns its address.
// The test is skipped when dbus-daemon is not installed.
func StartBus(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	conf := filepath.Join(dir, "session.conf")
	err = os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=`+sock+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config-file="+conf, "--nofork", "--nopidfile")
	if err := cmd.Start(); err != nil {
		t.Skipf("dbus-daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			return "unix:path=" + sock
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Skip("dbus-daemon did not start")
	return ""
}
