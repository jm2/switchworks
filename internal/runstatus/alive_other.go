// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix && !windows

package runstatus

import "time"

func alive(pid int, _ time.Time) bool { return pid > 0 }
