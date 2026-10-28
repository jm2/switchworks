// SPDX-License-Identifier: GPL-3.0-or-later

package codex

import (
	"reflect"
	"testing"
)

func TestParseStringArray(t *testing.T) {
	for in, want := range map[string][]string{
		`[]`:                                  {},
		`["/a", '/b\c']`:                      {"/a", `/b\c`},
		"[\n  \"/a\", # first\n  'C:\\x',\n]": {"/a", `C:\x`},
		`["C:\\Users\\u"] # trailing`:         {`C:\Users\u`},
	} {
		got, err := ParseStringArray(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{`"/a"`, `[1, 2]`, `["/a" "/b"]`, `[, "/a"]`, `["/a"`, `["""x"""]`} {
		if got, err := ParseStringArray(in); err == nil {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
}

func TestConfigWritableRoots(t *testing.T) {
	type res struct {
		roots   []string
		set, ok bool
	}
	for name, c := range map[string]struct {
		toml string
		want res
	}{
		"absent": {`model = "x"`, res{nil, false, true}},
		"table": {`
model = "x"
[sandbox_workspace_write]
network_access = false
writable_roots = ["/srv/a", "/srv/b"]
`, res{[]string{"/srv/a", "/srv/b"}, true, true}},
		"multi-line": {`
[sandbox_workspace_write]
writable_roots = [
  "/srv/a", # comment ]
  '/srv/b',
]
exclude_slash_tmp = true
`, res{[]string{"/srv/a", "/srv/b"}, true, true}},
		"dotted":       {`sandbox_workspace_write.writable_roots = ["/srv/a"]`, res{[]string{"/srv/a"}, true, true}},
		"inline table": {`sandbox_workspace_write = { writable_roots = ["/srv/a"] }`, res{nil, true, false}},
		"other table": {`
[profiles.x.sandbox_workspace_write]
writable_roots = ["/nope"]
`, res{nil, false, true}},
		"in a multi-line string": {`
instructions = """
[sandbox_workspace_write]
writable_roots = ["/nope"]
"""
`, res{nil, false, true}},
		"unreadable": {`
[sandbox_workspace_write]
writable_roots = [ "/a", 3 ]
`, res{nil, true, false}},
	} {
		roots, set, ok := ConfigWritableRoots([]byte(c.toml))
		if !reflect.DeepEqual(res{roots, set, ok}, c.want) {
			t.Errorf("%s: got %q %v %v, want %+v", name, roots, set, ok, c.want)
		}
	}
}

func TestConfigSetsEnv(t *testing.T) {
	for toml, want := range map[string]bool{
		``: false,
		"[shell_environment_policy.set]\nCCACHE_DIR = \"/c\"\n":            true,
		"[shell_environment_policy.set]\nccache_dir = \"/c\"\n":            true,
		"[shell_environment_policy.set]\nGIT_CONFIG_COUNT = \"1\"\n":       false,
		"[shell_environment_policy]\nset = { CCACHE_DIR = \"/c\" }\n":      true,
		"shell_environment_policy.set.CCACHE_DIR = \"/c\"\n":               true,
		"[shell_environment_policy]\ninherit = \"all\"\n":                  false,
		"[other.set]\nCCACHE_DIR = \"/c\"\n":                               false,
		"[shell_environment_policy]\nset.CCACHE_DIR = \"/c\"\n":            true,
		"[permissions.swk.filesystem]\n\"~/.ssh\" = \"deny\"\n":            false,
		"[shell_environment_policy.set]\nXDG_CACHE_HOME = \"/c\"\n":        false,
		"[shell_environment_policy.set]\n\"CCACHE_DIR\" = \"/c\" # mine\n": true,
	} {
		if got := ConfigSetsEnv([]byte(toml), "CCACHE_DIR"); got != want {
			t.Errorf("%q: %v, want %v", toml, got, want)
		}
	}
}

func TestConfigArgs(t *testing.T) {
	got := ConfigArgs([]string{"-c", "a.b=1", "--config=c=[\"/x\"]", "-cd=2", "-m", "-c", "app-server", "-c", "a.b=3", "--", "-c", "z=1"})
	want := map[string]string{"a.b": "3", "c": `["/x"]`, "d": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
