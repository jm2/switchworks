// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package agyad

import "syscall"

// oNoFollow makes opening a path that is a symlink fail.
const oNoFollow = syscall.O_NOFOLLOW
