// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix

package agyad

// oNoFollow is not available here; the Lstat checks before opening apply.
const oNoFollow = 0
