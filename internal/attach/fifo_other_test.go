// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix

package attach

import "errors"

func mkfifo(string) error { return errors.New("no FIFOs here") }
