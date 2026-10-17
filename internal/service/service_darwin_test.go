// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistText(t *testing.T) {
	for _, role := range Roles {
		s := plistText(role, "/Applications/Switch & yard.app/Contents/MacOS/swk", "", "/opt/homebrew/bin:/usr/bin",
			"/Users/dev/Library/Application Support/swk/node/logs/node.log")
		for _, want := range []string{
			"<string>dev.switchworks." + role + "</string>",
			"<string>/Applications/Switch &amp; yard.app/Contents/MacOS/swk</string>",
			"<string>--log-file</string>",
			"<key>KeepAlive</key>",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s plist lacks %q:\n%s", role, want, s)
			}
		}
		if strings.Contains(s, "UserName") || strings.Contains(s, "LaunchDaemons") {
			t.Errorf("%s plist is not a per-user agent:\n%s", role, s)
		}
		// plutil is on every Mac: the text must be a valid property list.
		if plutil, err := exec.LookPath("plutil"); err == nil {
			p := filepath.Join(t.TempDir(), "x.plist")
			if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
				t.Errorf("%s plist: plutil -lint: %v %s", role, err, out)
			}
		}
	}
}

// Bundled in the app, the agent starts the app's executable, which runs swk.
func TestPlistTextLauncher(t *testing.T) {
	s := plistText("node", "/Applications/Switchworks.app/Contents/MacOS/swk",
		"/Applications/Switchworks.app/Contents/MacOS/Switchworks", "/usr/bin", "/tmp/node.log")
	want := "<array>\n    <string>/Applications/Switchworks.app/Contents/MacOS/Switchworks</string>\n" +
		"    <string>--swk</string>\n    <string>node</string>\n    <string>run</string>"
	if !strings.Contains(s, want) {
		t.Fatalf("plist lacks %q:\n%s", want, s)
	}
}

func TestAppLauncher(t *testing.T) {
	macos := filepath.Join(t.TempDir(), "Switchworks.app", "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	swk := filepath.Join(macos, "swk")
	if got := appLauncher(swk); got != "" {
		t.Fatalf("launcher %q without the app's executable", got)
	}
	app := filepath.Join(macos, "Switchworks")
	if err := os.WriteFile(app, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := appLauncher(swk); got != app {
		t.Fatalf("appLauncher = %q, want %q", got, app)
	}
	if got := appLauncher("/opt/homebrew/bin/swk"); got != "" {
		t.Fatalf("launcher %q for a swk outside an app", got)
	}
}

func TestLaunchPATH(t *testing.T) {
	t.Setenv("SHELL", "")
	t.Setenv("PATH", ".:bin:/custom/bin::/usr/bin")
	got := strings.Split(launchPATH(), ":")
	for _, d := range got {
		if !filepath.IsAbs(d) {
			t.Fatalf("relative entry %q in %v", d, got)
		}
	}
	if got[0] != "/custom/bin" {
		t.Fatalf("PATH order: %v", got)
	}
}

func TestCheckExecutableRejectsVolatile(t *testing.T) {
	for _, p := range []string{"/Volumes/Switchworks/Switchworks.app/Contents/MacOS/swk",
		"/private/var/folders/x/AppTranslocation/ABC/d/Switchworks.app/Contents/MacOS/swk"} {
		if err := checkExecutable(p); err == nil {
			t.Errorf("checkExecutable(%q) accepted", p)
		}
	}
	if err := checkExecutable("/Applications/Switchworks.app/Contents/MacOS/swk"); err != nil {
		t.Error(err)
	}
}
