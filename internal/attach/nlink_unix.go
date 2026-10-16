// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package attach

import (
	"os"
	"syscall"
)

const oNoFollow = syscall.O_NOFOLLOW

func hardLinked(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
