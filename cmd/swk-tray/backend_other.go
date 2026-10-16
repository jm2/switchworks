// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !windows

package main

import (
	"errors"
	"fmt"

	"github.com/jm2/switchworks/internal/tray"
)

var errAlreadyRunning = errors.New("already running")

// On macOS the Switchworks menu-bar app (macos/Switchworks) is the tray.
func newBackend() (tray.Backend, error) {
	return nil, errors.New("no tray backend for this platform; on macOS use the Switchworks menu-bar app")
}

func reportFatal(err error) { fmt.Fprintf(stderr, "swk-tray: %v\n", err) }
