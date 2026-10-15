// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package tray

import (
	"errors"
	"github.com/jm2/switchyard/internal/paths"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AppID names the desktop file, the icon and the autostart entry.
const AppID = "io.github.jm2.Switchyard"

func init() {
	openURL = func(u string) error {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return errors.New("not a web address: " + u)
		}
		return xdgOpen(u)
	}
	openPath = xdgOpen
}

func xdgOpen(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

const autostartSupported = true

func autostartFile() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", AppID+".desktop")
}

func autostartEnabled() bool {
	b, err := os.ReadFile(autostartFile())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "Hidden=true" || line == "X-GNOME-Autostart-enabled=false" {
			return false
		}
	}
	return true
}

// desktopQuote quotes an Exec argument per the Desktop Entry specification.
func desktopQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\><~|&;$*?#()`=%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", "$", `\\$`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

func setAutostart(on bool) error {
	path := autostartFile()
	if !on {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	// A packaged tray is on PATH as swyd-tray; prefer the name so an
	// upgrade that moves the binary keeps working.
	if p, err := exec.LookPath("swyd-tray"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil && r == exe {
			exe = "swyd-tray"
		}
	}
	if strings.ContainsFunc(exe, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("the tray's path contains control characters; not writing an autostart entry")
	}
	text := "[Desktop Entry]\nType=Application\nName=Switchyard\n" +
		"Comment=Switchyard node status in the system tray\n" +
		"Exec=" + desktopQuote(exe) + "\nIcon=" + AppID + "\nTerminal=false\nNoDisplay=true\n" +
		"X-GNOME-Autostart-enabled=true\n"
	return paths.WriteFile(path, []byte(text), 0o644)
}
