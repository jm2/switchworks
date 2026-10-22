// SPDX-License-Identifier: GPL-3.0-or-later

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFlattenConfig(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"model_auto_compact_token_limit": 128000,
		"temperature": 0.5,
		"big": 1e21,
		"web_search": "disabled",
		"features": {"apps": false, "plugins": true},
		"windows.sandbox": "unelevated",
		"notify": ["say", "a \"b\"\\c\n\u0001"],
		"empty": {},
		"mcp_servers": {"x": {"env": {"A-B": "1"}}}
	}`), &m); err != nil {
		t.Fatal(err)
	}
	got, err := FlattenConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"model_auto_compact_token_limit": "128000",
		"temperature":                    "0.5",
		"big":                            "1e21",
		"web_search":                     `"disabled"`,
		"features.apps":                  "false",
		"features.plugins":               "true",
		"windows.sandbox":                `"unelevated"`,
		"notify":                         `["say", "a \"b\"\\c\n\u0001"]`,
		"empty":                          "{}",
		"mcp_servers.x.env.A-B":          `"1"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if got, want := OverrideArgs(map[string]string{"b": "1", "a.c": `"x"`}), []string{"-c", `a.c="x"`, "-c", "b=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args %q, want %q", got, want)
	}
}

func TestFlattenConfigRefuses(t *testing.T) {
	for _, in := range []string{
		`{"a": null}`,
		`{"a..b": 1}`,
		`{"a b": 1}`,
		`{"a=b": 1}`,
		`{"x": {"c d": "2"}}`,
		`{"features": {"apps": false}, "features.apps": true}`,
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatal(err)
		}
		if _, err := FlattenConfig(m); err == nil {
			t.Errorf("%s: accepted", in)
		}
	}
}

func TestEncodeTOMLInlineTable(t *testing.T) {
	s, err := EncodeTOML(map[string]any{"b": json.Number("2"), "a key": "v", "c": []any{true}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a key" = "v", b = 2, c = [true]}`; s != want {
		t.Fatalf("got %s, want %s", s, want)
	}
}

func TestArgsParsing(t *testing.T) {
	for _, c := range []struct {
		args []string
		sub  string
		keys []string
	}{
		{[]string{"app-server"}, "app-server", nil},
		{[]string{"-c", "a=1", "--config", "b.c=\"x\"", "--config=d=2", "-ce=3"}, "", []string{"a", "b.c", "d", "e"}},
		{[]string{"-c", "a=1", "-p", "work", "app-server", "-c", "f=1"}, "app-server", []string{"a", "f"}},
		{[]string{"--enable", "x", "--", "app-server"}, "app-server", nil},
		{[]string{"-C", "/x", "-cmodel=y"}, "", []string{"model"}},
	} {
		if got := Subcommand(c.args); got != c.sub {
			t.Errorf("Subcommand(%q) = %q, want %q", c.args, got, c.sub)
		}
		if got := ConfigKeys(c.args); !reflect.DeepEqual(got, c.keys) {
			t.Errorf("ConfigKeys(%q) = %q, want %q", c.args, got, c.keys)
		}
	}
}

func TestConfigSetsWindowsSandbox(t *testing.T) {
	for in, want := range map[string]bool{
		"": false,
		"model = \"x\"\n[windows]\n# sandbox = \"elevated\"\n":                        false,
		"[windows]\nsandbox = \"elevated\"\n":                                         true,
		"[ \"windows\" ]\n sandbox=\"unelevated\"":                                    true,
		"windows.sandbox = \"unelevated\"\n":                                          true,
		"windows = { sandbox = \"elevated\" }\n":                                      true,
		"[profiles.x.windows]\nsandbox = \"elevated\"\n":                              false,
		"[sandbox_workspace_write]\nnetwork_access = false\n":                         false,
		"[model_providers.x]\nwindows.sandbox = \"no\"\n":                             false,
		"[windows]\nother = 1\n[x]\nsandbox = 1\n":                                    false,
		"experimental_bearer_token = \"sk\"\n[windows]\r\nsandbox = \"elevated\"\r\n": true,
	} {
		if got := ConfigSetsWindowsSandbox([]byte(in)); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestProfileDenyPaths(t *testing.T) {
	toml := `default_permissions = "swk"
experimental_bearer_token = "deny"

[permissions.swk]
extends = ":workspace"
filesystem."~/.claude.json" = "deny"

[permissions.swk.network]
enabled = true

[permissions.swk.filesystem]
"~/.codex/auth.json" = "deny"   # Codex's login
'~/.config/gh' = 'deny'
"C:\\Users\\me\\.ssh" = "deny"
"~/work" = "write"
":workspace_roots" = "read"

[permissions.swk.filesystem.":workspace_roots"]
".git" = "deny"

[permissions.other.filesystem]
"/run/user/1000"="deny"` + "\r\n" + `
[[permissions.swk.filesystem]]
"~/.aws" = "deny"

[model_providers.x.filesystem]
"~/.x" = "deny"
permissions.top.filesystem."~/.gnupg" = "deny"
`
	got := ProfileDenyPaths([]byte(toml))
	want := []string{"~/.claude.json", "~/.codex/auth.json", "~/.config/gh", `C:\Users\me\.ssh`, "/run/user/1000", "~/.aws"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProfileDenyPaths = %q, want %q", got, want)
	}
	if got := ProfileDenyPaths([]byte("permissions.top.filesystem.\"~/.gnupg\" = \"deny\"\n[x]\n")); !reflect.DeepEqual(got, []string{"~/.gnupg"}) {
		t.Fatalf("top-level dotted key: %q", got)
	}
}

// A process that exits instead of answering initialize: its stderr comes
// back with the error, not in its message.
func TestStartErrorKeepsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, err := Start(context.Background(), Options{Bin: "sh", Args: []string{"-c", "echo 'error: unexpected argument' >&2; exit 2"}}, nil)
	var se *StartError
	if !errors.As(err, &se) {
		t.Fatalf("got %v, want a StartError", err)
	}
	if se.Stderr != "error: unexpected argument" || strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("stderr %q, message %q", se.Stderr, err)
	}
	tb := &tailBuffer{max: 4}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defg"))
	if tb.String() != "defg" {
		t.Fatalf("tail %q", tb.String())
	}
}
