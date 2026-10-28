// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps git from opening a console window.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
