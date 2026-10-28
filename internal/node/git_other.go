// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package node

import "os/exec"

// hideWindow keeps git from opening a console window: nothing to do
// outside Windows.
func hideWindow(*exec.Cmd) {}
