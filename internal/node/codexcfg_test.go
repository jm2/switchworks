// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/harness"
)

func nodeConfig(t *testing.T, js string) Config {
	t.Helper()
	var c Config
	if err := json.Unmarshal([]byte(js), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCodexConfig(t *testing.T) {
	// The issue's example works, and lands ahead of the default subcommand.
	c := nodeConfig(t, `{"codex": {"config": {"model_auto_compact_token_limit": 128000}}, "unknown": 1}`)
	over, err := codexConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	argv := harness.CodexArgv(harness.Options{CodexArgs: c.Codex.Args, CodexConfig: over})
	if want := []string{"-c", "model_auto_compact_token_limit=128000", "app-server"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv %q, want %q", argv, want)
	}
	// Without codex settings, nothing changes.
	if over, err := codexConfig(nodeConfig(t, `{}`)); err != nil || len(over) != 0 {
		t.Fatalf("empty config: %v %v", over, err)
	}
	if argv := harness.CodexArgv(harness.Options{}); !reflect.DeepEqual(argv, []string{"app-server"}) {
		t.Fatalf("default argv %q", argv)
	}
}

func TestCodexLean(t *testing.T) {
	// Off by default.
	if over, _ := codexConfig(nodeConfig(t, `{"codex": {"config": {"x": 1}}}`)); len(over) != 1 {
		t.Fatalf("lean defaults without lean: %v", over)
	}
	over, err := codexConfig(nodeConfig(t, `{"codex": {"lean": true}}`))
	if err != nil || !reflect.DeepEqual(over, leanCodexConfig) {
		t.Fatalf("lean: %v %v", over, err)
	}
	// What the user sets wins: in codex.config, a table holding a key, or
	// a -c in args.
	over, err = codexConfig(nodeConfig(t, `{"codex": {"lean": true,
		"args": ["-c", "web_search=\"live\"", "app-server"],
		"config": {"features": {"apps": true}, "model_auto_compact_token_limit": 200000, "tool_output_token_limit": {"x": 1}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"features.apps": "true", "model_auto_compact_token_limit": "200000",
		"features.plugins": "false", "tool_output_token_limit.x": "1"} {
		if over[k] != want {
			t.Errorf("%s = %q, want %q", k, over[k], want)
		}
	}
	for _, k := range []string{"web_search", "tool_output_token_limit"} {
		if _, ok := over[k]; ok {
			t.Errorf("lean overrode %s, which the user set", k)
		}
	}
	for k := range leanCodexConfig {
		if strings.Contains(k, "reasoning") {
			t.Errorf("lean sets %s", k)
		}
	}
}

func TestCodexConfigRefuses(t *testing.T) {
	for js, want := range map[string]string{
		// antec, 2026-10-21: args with only overrides dropped app-server.
		`{"codex": {"args": ["-c", "model_auto_compact_token_limit=128000"]}}`:                                   "no subcommand",
		`{"codex": {"args": ["-c", "web_search=\"disabled\"", "app-server"], "config": {"web_search": "live"}}}`: "both",
		`{"codex": {"config": {"bad key": 1}}}`:                                                                  "bad key",
		`{"codex": {"config": {"x": null}}}`:                                                                     "null",
	} {
		_, err := codexConfig(nodeConfig(t, js))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error about %q", js, err, want)
		}
	}
	// A secret in an override is not repeated in the error.
	_, err := codexConfig(nodeConfig(t, `{"codex": {"args": ["-c", "experimental_bearer_token=\"sk-secret\""]}}`))
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("got %v", err)
	}
}

func TestWindowsSandboxDefault(t *testing.T) {
	over := map[string]string{}
	if !windowsSandboxDefault(over, nil, nil) || over["windows.sandbox"] != `"unelevated"` {
		t.Fatalf("no config: %v", over)
	}
	for name, c := range map[string]struct {
		over map[string]string
		args []string
		toml string
	}{
		"config.toml table":  {toml: "[windows]\nsandbox = \"elevated\"\n"},
		"config.toml dotted": {toml: "windows.sandbox = \"unelevated\"\n"},
		"codex.config":       {over: map[string]string{"windows.sandbox": `"elevated"`}},
		"codex.config table": {over: map[string]string{"windows": `{sandbox = "elevated"}`}},
		"codex.args":         {args: []string{"-c", `windows.sandbox="elevated"`, "app-server"}},
	} {
		over := c.over
		if over == nil {
			over = map[string]string{}
		}
		before := len(over)
		if windowsSandboxDefault(over, c.args, []byte(c.toml)) || len(over) != before {
			t.Errorf("%s: default added: %v", name, over)
		}
	}
}

func TestIsAppExecAlias(t *testing.T) {
	const lad = `C:\Users\me\AppData\Local`
	for _, c := range []struct {
		path string
		size int64
		want bool
	}{
		{`C:\Users\me\AppData\Local\Microsoft\WindowsApps\pwsh.exe`, 0, true},
		{`c:\users\ME\appdata\local\microsoft\windowsapps\pwsh.exe`, -1, true},
		{`C:\Users\me\AppData\Local/Microsoft/WindowsApps/pwsh.exe`, 1234, true},
		{`C:\Program Files\PowerShell\7\pwsh.exe`, 288768, false},
		{`C:\Program Files\PowerShell\7\pwsh.exe`, -1, false},
		{`C:\tools\pwsh.exe`, 0, true},
		{`C:\Users\me\AppData\Local\Microsoft\WindowsAppsX\pwsh.exe`, 100, false},
	} {
		if got := isAppExecAlias(c.path, c.size, lad); got != c.want {
			t.Errorf("isAppExecAlias(%q, %d) = %v, want %v", c.path, c.size, got, c.want)
		}
	}
	if isAppExecAlias(`C:\Program Files\PowerShell\7\pwsh.exe`, 5, "") {
		t.Error("alias without LOCALAPPDATA")
	}
	if w := pwshWarning(`C:\x\pwsh.exe`); !strings.Contains(w, "MSI") || !strings.Contains(w, `C:\x\pwsh.exe`) {
		t.Errorf("warning %q", w)
	}
}
