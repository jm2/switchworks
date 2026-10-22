// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"errors"
	"fmt"

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
	for _, k := range codex.ConfigKeys(cfg.Codex.Args) {
		if _, ok := over[k]; ok {
			return nil, fmt.Errorf("node.json sets Codex config %q in both codex.args (-c) and codex.config; keep one", k)
		}
	}
	return over, nil
}
