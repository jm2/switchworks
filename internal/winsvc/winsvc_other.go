// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package winsvc

import "context"

func run(func(context.Context) int) (int, error) { return 1, ErrNotService }
