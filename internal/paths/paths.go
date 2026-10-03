// Package paths resolves Switchyard's default file locations.
//
// Everything lives under one base directory: $SWYD_HOME if set, otherwise
// os.UserConfigDir()/swyd (~/.config/swyd on Linux, ~/Library/Application
// Support/swyd on macOS, %AppData%\swyd on Windows).
package paths

import (
	"os"
	"path/filepath"

	"github.com/jm2/switchyard/internal/version"
)

// Base returns the base directory.
func Base() string {
	if v := os.Getenv("SWYD_HOME"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "."+version.Bin)
	}
	return filepath.Join(dir, version.Bin)
}

// CoordinatorDir holds the CA, server certificate, config and data.
func CoordinatorDir() string { return filepath.Join(Base(), "coordinator") }

// NodeDir holds the node's certificate, config, state and spool.
func NodeDir() string { return filepath.Join(Base(), "node") }

// CLIConfig is the CLI's connection file ($SWYD_CLI_CONFIG overrides).
func CLIConfig() string {
	if v := os.Getenv("SWYD_CLI_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(Base(), "cli.json")
}
