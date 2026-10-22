// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows && !linux

package proc

// waitExited cannot wait without reaping here (the syscall package has no
// waitid), so wait reaps first and ends the group after.
func waitExited(pid int) bool { return false }
