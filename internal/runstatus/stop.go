// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// StopContext returns a context that ends when the process is asked to stop:
// on SIGINT or SIGTERM everywhere and, on Windows, also when RequestStop is
// called for the same dir. Windows cannot deliver a console signal to a
// process without a console, such as the swydw.exe a logon task runs, so
// `swyd service stop` uses the stop event there.
func StopContext(parent context.Context, dir string) (context.Context, context.CancelFunc) {
	return stopContext(parent, dir)
}

// RequestStop asks the process running from dir to stop gracefully. It
// returns errors.ErrUnsupported where the service manager's own stop is
// already graceful (everywhere but Windows).
func RequestStop(dir string) error { return requestStop(dir) }

// stopName names the per-directory stop event. Windows "Local\" objects are
// private to the logon session.
func stopName(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return `Local\swyd-stop-` + hex.EncodeToString(sum[:8])
}
