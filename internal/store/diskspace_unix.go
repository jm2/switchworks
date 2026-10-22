// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd

package store

import "syscall"

// DiskSpace returns the bytes free (to an unprivileged user) and in total
// on the volume that holds dir.
func DiskSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// The field types differ between systems.
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}
