// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix

package acpad

import "testing"

func processAlive(t *testing.T, _ int) bool {
	t.Skip("process liveness check is Unix-only")
	return false
}
