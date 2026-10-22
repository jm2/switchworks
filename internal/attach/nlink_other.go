// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix && !windows

package attach

import "os"

const oNoFollow = 0

// hardLinked is not checked here: there is no portable link count.
func hardLinked(os.FileInfo) bool { return false }

func openHardLinked(*os.File, os.FileInfo) (bool, error) { return false, nil }
