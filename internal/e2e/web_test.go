// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/model"
)

// TestFleetStreamActivity checks what the web UI's fleet view is built
// from: the fleet stream's snapshot and updates, and the activity (last
// message, open approvals) on session views.
func TestFleetStreamActivity(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer func() { co.stop() }()
	c := apiClient(t, coordDir)

	var mu sync.Mutex
	var got []string
	sessions := map[string]apiclient.SessionView{}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = c.FleetStream(sctx, func(typ string, data json.RawMessage) bool {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, typ)
			if typ == "session" {
				var v apiclient.SessionView
				_ = json.Unmarshal(data, &v)
				sessions[v.ID] = v
			}
			return true
		})
	}()
	waitFleet := func(what string, pred func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			ok := pred()
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("fleet stream: no %s; got %v", what, got)
	}
	waitFleet("hello", func() bool { return len(got) > 0 && got[0] == "hello" })

	_, _, nd := enrollHost(t, c, "devbox")
	defer func() { nd.stop(t) }()
	waitFleet("host online", func() bool {
		for _, g := range got {
			if g == "host" {
				return true
			}
		}
		return false
	})

	sess, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "web", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitFleet("idle session", func() bool { return sessions[sess.ID].Status == model.StatusIdle })

	if _, err := c.Send(ctx, sess.ID, "APPROVE deploy", "queue", ""); err != nil {
		t.Fatal(err)
	}
	waitFleet("open approval", func() bool { return len(sessions[sess.ID].Approvals) == 1 })
	v, _ := c.Session(ctx, sess.ID)
	if len(v.Approvals) != 1 || v.Approvals[0].Command != "make test" {
		t.Fatalf("session view approvals: %+v", v.Approvals)
	}
	if err := c.Approve(ctx, sess.ID, v.Approvals[0].ApprovalID, "accept"); err != nil {
		t.Fatal(err)
	}
	waitFleet("approval cleared and preview", func() bool {
		s := sessions[sess.ID]
		return len(s.Approvals) == 0 && s.Preview == "approval decision: accept" && s.Status == model.StatusIdle
	})

	// Activity is rebuilt from the log when the coordinator restarts.
	apiAddr, nodeAddr := co.apiAddr, co.nodeAdd
	cancel()
	co.stop()
	co = startCoordinator(t, coordDir, apiAddr, nodeAddr)
	waitOnline(t, c, "devbox")
	v, err = c.Session(ctx, sess.ID)
	if err != nil || v.Preview != "approval decision: accept" || len(v.Approvals) != 0 {
		t.Fatalf("after restart: %+v %v", v, err)
	}
}
