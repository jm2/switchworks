// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package runstatus

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

func stopContext(parent context.Context, _ string) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func requestStop(string) error { return errors.ErrUnsupported }
