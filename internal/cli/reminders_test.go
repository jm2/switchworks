// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
)

// The table shows each reminder's id, when it is due and the agent's own
// text (without the header it is delivered with), on one line.
func TestPrintReminders(t *testing.T) {
	var b bytes.Buffer
	printReminders(&b, "devbox/a", nil)
	if !strings.Contains(b.String(), "devbox/a has no pending reminders") {
		t.Fatalf("empty:\n%s", b.String())
	}
	b.Reset()
	printReminders(&b, "devbox/a", []fleet.Reminder{
		{ID: "ci:123", MessageID: "m1", Text: "Reminder \"ci:123\", which you set 10m ago with fleet_remind:\ncheck CI\nthen merge", DueAt: time.Now().Add(10 * time.Minute)},
		{ID: "long", MessageID: "m2", Text: "Reminder \"long\":\n" + strings.Repeat("é", 100), DueAt: time.Now().Add(time.Hour)},
	})
	out := b.String()
	if !strings.Contains(out, "ci:123") || !strings.Contains(out, "check CI then merge") || strings.Contains(out, "which you set") ||
		!strings.Contains(out, strings.Repeat("é", 77)+"...") || strings.Contains(out, strings.Repeat("é", 78)) {
		t.Fatalf("table:\n%s", out)
	}
}
