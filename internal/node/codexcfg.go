// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jm2/switchworks/internal/codex"
)

// codexConfig checks node.json's codex settings and returns the config
// overrides the node passes to codex as -c key=value (dotted key -> TOML
// value). Values are never logged: config can hold provider keys.
func codexConfig(cfg Config) (map[string]string, error) {
	if len(cfg.Codex.Args) > 0 && codex.Subcommand(cfg.Codex.Args) == "" {
		// Without one codex does not run app-server, and every session
		// fails with "app-server initialize: connection closed".
		return nil, errors.New(`node.json codex.args has no subcommand: it replaces codex's whole argument list, so it must include one (normally "app-server"); put config overrides in codex.config instead`)
	}
	over, err := codex.FlattenConfig(cfg.Codex.Config)
	if err != nil {
		return nil, fmt.Errorf("node.json %w", err)
	}
	argKeys := codex.ConfigKeys(cfg.Codex.Args)
	for _, k := range argKeys {
		if _, ok := over[k]; ok {
			return nil, fmt.Errorf("node.json sets Codex config %q in both codex.args (-c) and codex.config; keep one", k)
		}
	}
	if cfg.Codex.Lean {
		addDefaults(over, argKeys, leanCodexConfig)
	}
	return over, nil
}

// leanCodexConfig is what codex.lean adds. A managed session pays for
// every tool Codex offers on every model call: on 2026-10-21 the first
// call of a fresh thread sent 47.5K input tokens with Codex's defaults and
// 12.8K with these tools off. The apps feature is also the ChatGPT GitHub
// connector, which lets an agent act on GitHub as the owner. Codex's
// fallback metadata for a model it does not know (deepseek-flash, say)
// gives a 258,400-token window that compacts at about 245K; 128K compacts
// sooner. Reasoning effort is the user's choice and is left alone.
var leanCodexConfig = map[string]string{
	"features.apps":                  "false",
	"features.browser_use":           "false",
	"features.browser_use_external":  "false",
	"features.computer_use":          "false",
	"features.image_generation":      "false",
	"features.in_app_browser":        "false",
	"features.plugins":               "false",
	"web_search":                     `"disabled"`,
	"tool_output_token_limit":        "1500",
	"model_auto_compact_token_limit": "128000",
}

// windowsSandboxKey is the Codex setting that picks the Windows sandbox.
// Without it, Codex 0.163 cannot give a thread workspace-write on Windows
// and runs it read-only without saying so; under approval "never" every
// command is then rejected ("blocked by policy"). The unelevated sandbox
// needs no administrator setup.
const windowsSandboxKey = "windows.sandbox"

// windowsSandboxDefault adds windows.sandbox="unelevated" to over unless
// node.json (over, or a -c in args) or config.toml (configTOML, nil when
// absent) sets a Windows sandbox mode. It reports whether it added it.
func windowsSandboxDefault(over map[string]string, args []string, configTOML []byte) bool {
	if codex.ConfigSetsWindowsSandbox(configTOML) {
		return false
	}
	before := len(over)
	addDefaults(over, codex.ConfigKeys(args), map[string]string{windowsSandboxKey: `"unelevated"`})
	return len(over) > before
}

// isAppExecAlias reports whether path, as exec.LookPath found it, is a
// Microsoft Store App Execution Alias: a 0-byte placeholder (size 0) or
// anything in %LOCALAPPDATA%\Microsoft\WindowsApps. A process with a
// restricted token, such as one in Codex's Windows sandbox, cannot launch
// one ("CreateProcessAsUserW failed: 5"). size is -1 when unknown.
func isAppExecAlias(path string, size int64, localAppData string) bool {
	if size == 0 {
		return true
	}
	if localAppData == "" {
		return false
	}
	norm := func(p string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(p, "/", `\`)), `\`) }
	return strings.HasPrefix(norm(path), norm(localAppData)+`\microsoft\windowsapps\`)
}

// pwshWarning is logged at node start on Windows when pwsh is an alias.
func pwshWarning(path string) string {
	return fmt.Sprintf("warning: pwsh on PATH is %s, a Microsoft Store App Execution Alias. Codex runs commands with pwsh, "+
		"and its sandbox cannot start the Store version (\"CreateProcessAsUserW failed: 5\"), so every command fails. "+
		"Install PowerShell 7 from the MSI (https://aka.ms/powershell-release?tag=stable, or "+
		"winget install --id Microsoft.PowerShell --source winget) and restart the node", path)
}

// addDefaults adds each of defaults to over unless the user already set
// that key, a table containing it, or a key inside it, in over or with a
// -c in args (argKeys).
func addDefaults(over map[string]string, argKeys []string, defaults map[string]string) {
	set := make([]string, 0, len(over)+len(argKeys))
	for k := range over {
		set = append(set, k)
	}
	set = append(set, argKeys...)
next:
	for k, v := range defaults {
		for _, s := range set {
			if s == k || strings.HasPrefix(k, s+".") || strings.HasPrefix(s, k+".") {
				continue next
			}
		}
		over[k] = v
	}
}
