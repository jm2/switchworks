// SPDX-License-Identifier: GPL-3.0-or-later

package runstatus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked means another process already runs from the directory.
var ErrLocked = errors.New("already running")

// Lock takes dir's run lock, held until Close (or the process exits), so
// two copies of the node or coordinator never share one state directory:
// they would resume the same sessions, push each other off the
// coordinator and corrupt each other's spool.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrLocked) {
			pid := "another process"
			if s, err := Read(dir); err == nil && s.PID > 0 {
				pid = fmt.Sprintf("process %d", s.PID)
			}
			return nil, fmt.Errorf("%s: %w (%s); stop it first (swyd service stop, or end that process)", dir, ErrLocked, pid)
		}
		return nil, err
	}
	return f, nil
}
