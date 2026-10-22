// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !freebsd && !windows

package store

import "errors"

// DiskSpace is not implemented on this system.
func DiskSpace(dir string) (free, total uint64, err error) {
	return 0, 0, errors.ErrUnsupported
}
