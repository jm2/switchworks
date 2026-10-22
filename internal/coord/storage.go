// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"time"

	"github.com/jm2/switchworks/internal/store"
)

// maxEventLogMB caps event_log_max_mb (1 TiB), short of overflowing.
const maxEventLogMB = 1 << 20

// eventLogCap returns the per-session event log cap in bytes (0: none).
func (c Config) eventLogCap() int64 {
	switch mb := c.EventLogMaxMB; {
	case mb < 0:
		return 0
	case mb == 0:
		return store.DefaultEventLogCap
	default:
		return int64(min(mb, maxEventLogMB)) << 20
	}
}

// diskCheckEvery is how often watchDisk looks at the data volume, and
// diskWarnEvery how often it says again that it is still low.
const (
	diskCheckEvery = 5 * time.Minute
	diskWarnEvery  = time.Hour
)

// lowDisk reports whether free bytes of total are too few to go on safely:
// under 5% of the volume, but never under 1 GiB or over 10 GiB.
func lowDisk(free, total uint64) bool {
	floor := min(max(total/20, 1<<30), 10<<30)
	return free < floor
}

// watchDisk checks the free space on the data volume until ctx ends: when
// it runs out, event appends fail and nodes keep (and grow) their spools.
// It logs a warning when space runs low (again every diskWarnEvery while
// it stays low) and shows it in /v1/health.
func (s *Server) watchDisk(ctx context.Context, dir string) {
	var warned time.Time
	t := time.NewTicker(diskCheckEvery)
	defer t.Stop()
	for {
		warned = s.checkDisk(dir, time.Now(), warned)
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// checkDisk is one watchDisk check; warned is when it last warned, and it
// returns the same, updated.
func (s *Server) checkDisk(dir string, now, warned time.Time) time.Time {
	free, total, err := store.DiskSpace(dir)
	if err != nil {
		return warned // not supported here, or the directory is gone
	}
	return s.noteDisk(dir, free, total, now, warned)
}

func (s *Server) noteDisk(dir string, free, total uint64, now, warned time.Time) time.Time {
	low := lowDisk(free, total)
	was := s.diskLow.Swap(low)
	switch {
	case low && (!was || now.Sub(warned) >= diskWarnEvery):
		s.logf("WARNING: low disk space for %s: %d MiB free of %d MiB; free some, or lower session_retention_days or event_log_max_mb in coordinator.json", dir, free>>20, total>>20)
		return now
	case !low && was:
		s.logf("disk space for %s is no longer low: %d MiB free", dir, free>>20)
	}
	return warned
}
