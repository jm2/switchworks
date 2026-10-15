// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix && !windows

package runstatus

import "os"

func lockFile(*os.File) error { return nil }
