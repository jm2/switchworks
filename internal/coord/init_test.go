// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// init leaves this user's CLI config alone unless asked: agents running as
// this user could read the admin token there.
func TestInitUserCLIOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"default", nil, "", false},
		{"no-user-cli still accepted", []string{"--no-user-cli"}, "", false},
		{"user-cli", []string{"--user-cli"}, "", true},
		{"no-user-cli wins", []string{"--user-cli", "--no-user-cli"}, "", false},
		{"container image", nil, "SWK_USER_CLI", true},
		{"system coordinator", nil, "SWK_SYSTEM", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			userCLI := filepath.Join(home, "cli.json")
			t.Setenv("SWK_HOME", home)
			t.Setenv("SWK_CLI_CONFIG", userCLI)
			t.Setenv("SWK_SYSTEM", "")
			t.Setenv("SWK_USER_CLI", "")
			if tc.env != "" {
				t.Setenv(tc.env, "1")
			}
			var out, errb bytes.Buffer
			args := append([]string{"--dir", filepath.Join(home, "coordinator"), "--host", "127.0.0.1"}, tc.args...)
			if code := mainInit(args, &out, &errb); code != 0 {
				t.Fatalf("init: %d %s", code, errb.String())
			}
			_, err := os.Stat(userCLI)
			if got := err == nil; got != tc.want {
				t.Fatalf("user cli.json installed: %v, want %v\n%s", got, tc.want, out.String())
			}
			// Every hint for a machine where agents run offers a read token.
			if strings.Contains(out.String(), "--scope operate") {
				t.Fatalf("init recommends an operate token:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "--scope read") {
				t.Fatalf("init does not recommend a read token:\n%s", out.String())
			}
		})
	}
}
