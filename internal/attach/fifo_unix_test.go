// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package attach

import "syscall"

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }
