// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package acpad

import (
	"syscall"
	"testing"
)

func processAlive(_ *testing.T, pid int) bool { return syscall.Kill(pid, 0) == nil }
