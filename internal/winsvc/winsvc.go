// SPDX-License-Identifier: GPL-3.0-or-later

// Package winsvc runs a program as a Windows service: it connects to the
// service control manager, reports the service running, and cancels the
// body's context when Windows asks the service to stop (or shuts down).
package winsvc

import (
	"context"
	"errors"
)

// ErrNotService is returned when the process was not started by the service
// control manager.
var ErrNotService = errors.New("not started by the Windows service control manager")

// Run runs body as the service, until it returns, and returns its exit
// code. It returns ErrNotService elsewhere than in a Windows service.
func Run(body func(ctx context.Context) int) (int, error) { return run(body) }
