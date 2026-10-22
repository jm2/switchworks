// SPDX-License-Identifier: GPL-3.0-or-later

// Package paths resolves Switchworks's default file locations.
//
// Everything lives under one base directory: $SWK_HOME if set, otherwise
// os.UserConfigDir()/swk (~/.config/swk on Linux, ~/Library/Application
// Support/swk on macOS, %AppData%\swk on Windows).
package paths

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/jm2/switchworks/internal/version"
)

// Base returns the base directory.
func Base() string {
	if v := os.Getenv("SWK_HOME"); v != "" {
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

// CLIConfig is the CLI's connection file ($SWK_CLI_CONFIG overrides).
func CLIConfig() string {
	if v := os.Getenv("SWK_CLI_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(Base(), "cli.json")
}

// WriteFile replaces path with data, readable only as mode allows: it
// writes a new file and renames it into place, so an existing file's looser
// permissions, or a symlink at path, are never reused. Parent directories
// are created 0700. The file and then its directory are synced, so once it
// returns the new contents survive a power loss (a sign-out, say, cannot
// come undone).
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after a successful rename this is a no-op
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename in dir durable. Windows cannot sync a directory
// (and does not need to).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
