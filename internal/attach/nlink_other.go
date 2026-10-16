// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix

package attach

import "os"

const oNoFollow = 0

// hardLinked is not checked here: Lstat does not report link counts on
// Windows.
func hardLinked(os.FileInfo) bool { return false }
