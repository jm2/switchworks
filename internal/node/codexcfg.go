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
