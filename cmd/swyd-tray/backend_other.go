// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !windows

package main

import (
	"errors"
	"fmt"

	"github.com/jm2/switchyard/internal/tray"
)

var errAlreadyRunning = errors.New("already running")

// On macOS the Switchyard menu-bar app (macos/Switchyard) is the tray.
func newBackend() (tray.Backend, error) {
	return nil, errors.New("no tray backend for this platform; on macOS use the Switchyard menu-bar app")
}

func reportFatal(err error) { fmt.Fprintf(stderr, "swyd-tray: %v\n", err) }
