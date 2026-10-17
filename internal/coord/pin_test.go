// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
)

func TestUnpinnedHostsLogged(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, []string{"127.0.0.1"}, 7480, 7481); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenFileStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertHost(model.Host{ID: "oldbox"})
	_ = st.UpsertHost(model.Host{ID: "newbox", CertSHA256: "ab"})
	var mu sync.Mutex
	var logs []string
	s, err := NewServer(dir, cfg, st, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	mu.Lock()
	all := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(all, "host oldbox has no pinned certificate") || strings.Contains(all, "host newbox has no") {
		t.Fatalf("logs: %s", all)
	}
}
