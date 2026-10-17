// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package service

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSystemUnitText(t *testing.T) {
	b, err := systemUnitText("/usr/local/lib/switchworks/swk")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"ExecStart=/usr/local/lib/switchworks/swk coordinator run\n",
		"User=" + SystemAccount + "\n",
		"Environment=SWK_HOME=" + systemHomeDir + "\n",
		"StateDirectory=switchworks\n",
		"CapabilityBoundingSet=\n",
		"ProtectHome=yes\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("system unit lacks %q:\n%s", want, s)
		}
	}
	if _, err := systemUnitText("/opt/a\nExecStartPre=/bin/evil"); err == nil {
		t.Error("systemUnitText accepted a newline")
	}
}

// The account the packages declare is the one the code uses.
func TestSysusersLine(t *testing.T) {
	var fields []string
	for _, l := range strings.Split(sysusersConf, "\n") {
		if strings.HasPrefix(l, "u ") {
			fields = strings.Fields(l)
		}
	}
	if len(fields) < 2 || fields[1] != SystemAccount || fields[len(fields)-1] != systemHomeDir {
		t.Fatalf("sysusers line %q does not declare %s with home %s", fields, SystemAccount, systemHomeDir)
	}
}

func TestRootOnly(t *testing.T) {
	if !rootOnly("/") {
		t.Skip("/ is not root-owned here")
	}
	f := filepath.Join(t.TempDir(), "swk")
	if err := os.WriteFile(f, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && rootOnly(f) {
		t.Errorf("rootOnly(%s) = true for a file this user owns", f)
	}
	if rootOnly("/tmp") {
		t.Error("rootOnly(/tmp) = true for a world-writable directory")
	}
	if rootOnly("/no/such/file") {
		t.Error("rootOnly accepted a missing path")
	}
}

func TestSystemEnv(t *testing.T) {
	u := &user.User{Username: SystemAccount}
	env := systemEnv([]string{
		"LANG=C.UTF-8", "LC_ALL=C", "TERM=xterm", "SSH_AUTH_SOCK=/run/user/1000/ssh",
		"SWK_HOME=/home/me/.config/swk", "SWK_CLI_CONFIG=/home/me/cli.json", "LD_PRELOAD=/tmp/x.so", "PATH=/home/me/bin",
	}, u)
	for _, want := range []string{"SWK_HOME=" + systemHomeDir, "HOME=" + systemHomeDir, "LANG=C.UTF-8", "LC_ALL=C", "TERM=xterm", "USER=" + SystemAccount} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %q: %q", want, env)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "SSH_AUTH_SOCK", "SWK_CLI_CONFIG", "LD_PRELOAD":
			t.Errorf("env passes the caller's %s: %q", k, env)
		}
		if kv == "PATH=/home/me/bin" || kv == "SWK_HOME=/home/me/.config/swk" {
			t.Errorf("env keeps the caller's %q", kv)
		}
	}
}

// The rpm's %pre creates the account from a copy of the sysusers.d line.
func TestSpecSysusersLine(t *testing.T) {
	spec, err := os.ReadFile("../../build-aux/rpm/switchworks.spec")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(sysusersConf, "\n") {
		if strings.HasPrefix(l, "u ") && !strings.Contains(string(spec), "\n"+l+"\n") {
			t.Errorf("the spec's %%pre lacks %q", l)
		}
	}
}
