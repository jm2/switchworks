// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/jm2/switchworks/internal/tray"
	"github.com/jm2/switchworks/internal/tray/sni"
)

var errAlreadyRunning = sni.ErrAlreadyRunning

func newBackend() (tray.Backend, error) { return sni.New(), nil }

func reportFatal(err error) { fmt.Fprintf(stderr, "swk-tray: %v\n", err); _ = os.Stderr.Sync() }
