// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"os"
	"syscall"
)

const oNoFollow = 0

// hardLinked is false here: Lstat does not report link counts on
// Windows. openHardLinked asks the open file instead.
func hardLinked(os.FileInfo) bool { return false }

// openHardLinked reports whether the open file f has other hard links,
// from GetFileInformationByHandle's link count. A file it cannot ask
// about counts as linked.
func openHardLinked(f *os.File, _ os.FileInfo) (bool, error) {
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return true, err
	}
	return d.NumberOfLinks > 1, nil
}
