// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os"
	"testing"

	"github.com/jm2/switchworks/internal/codex/codextest"
)

// The test binary doubles as a fake codex app-server (codextest).
func TestMain(m *testing.M) {
	if codextest.MaybeRun() {
		return
	}
	os.Exit(m.Run())
}
