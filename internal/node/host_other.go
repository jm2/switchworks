// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package node

// hostCodexConfig adds this platform's defaults to Codex's overrides:
// none outside Windows.
func hostCodexConfig(Config, map[string]string, func(string, ...any)) {}

// checkCodexHost checks what Codex needs from this host: nothing outside
// Windows.
func checkCodexHost(func(string, ...any)) {}
