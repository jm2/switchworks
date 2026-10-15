// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !windows

package tray

import "errors"

const autostartSupported = false

func autostartEnabled() bool { return false }

func setAutostart(bool) error { return errors.New("not supported on this platform") }
