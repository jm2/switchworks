// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/store"
)

func TestEventLogCapConfig(t *testing.T) {
	for _, c := range []struct {
		mb   int
		want int64
	}{
		{0, store.DefaultEventLogCap},
		{-1, 0},
		{1, 1 << 20},
		{512, 512 << 20},
		{1 << 30, maxEventLogMB << 20},
	} {
		if got := (Config{EventLogMaxMB: c.mb}).eventLogCap(); got != c.want {
			t.Errorf("event_log_max_mb %d: cap %d, want %d", c.mb, got, c.want)
		}
	}
}

func TestLowDisk(t *testing.T) {
	const gib = 1 << 30
	for _, c := range []struct {
		free, total uint64
		want        bool
	}{
		{gib / 2, 4 * gib, true},      // under the 1 GiB floor
		{2 * gib, 4 * gib, false},     // 50%
		{4 * gib, 100 * gib, true},    // 4%
		{6 * gib, 100 * gib, false},   // 6%
		{11 * gib, 3000 * gib, false}, // 5% would be 150 GiB: capped at 10
		{9 * gib, 3000 * gib, true},
	} {
		if got := lowDisk(c.free, c.total); got != c.want {
			t.Errorf("lowDisk(%d GiB of %d GiB) = %v", c.free/gib, c.total/gib, got)
		}
	}
}

// Low space is logged when it starts, again hourly, and when it ends, and
// /v1/health says so while it lasts.
func TestDiskWarning(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv
	var logs []string
	s.logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	health := func() map[string]any {
		t.Helper()
		resp := e.do(t, "GET", "/v1/health", "", nil)
		defer resp.Body.Close()
		var h map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	const gib = 1 << 30
	now := time.Now()
	var warned time.Time
	warned = s.noteDisk("data", 50*gib, 100*gib, now, warned)
	if len(logs) != 0 || health()["storage_low"] != nil {
		t.Fatalf("plenty of space: %q %v", logs, health())
	}
	warned = s.noteDisk("data", gib/2, 100*gib, now, warned)
	if len(logs) != 1 || !strings.Contains(logs[0], "low disk space") || health()["storage_low"] != true {
		t.Fatalf("low: %q %v", logs, health())
	}
	warned = s.noteDisk("data", gib/2, 100*gib, now.Add(time.Minute), warned)
	if len(logs) != 1 {
		t.Fatalf("warned again within the hour: %q", logs)
	}
	warned = s.noteDisk("data", gib/2, 100*gib, now.Add(diskWarnEvery), warned)
	if len(logs) != 2 {
		t.Fatalf("not warned again after an hour: %q", logs)
	}
	s.noteDisk("data", 50*gib, 100*gib, now.Add(diskWarnEvery+time.Minute), warned)
	if len(logs) != 3 || !strings.Contains(logs[2], "no longer low") || health()["storage_low"] != nil {
		t.Fatalf("recovered: %q %v", logs, health())
	}
}
