// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package gpurun

import "context"

// Run is only implemented on Windows.
func Run(ctx context.Context, p Plan) (Result, error) { return Result{}, ErrUnsupported }
