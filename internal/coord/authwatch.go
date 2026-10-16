// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"sync"
	"time"
)

// authRecheck is how often open requests (streams, long waits) check that
// their credential is still good. Revoking a token or signing out checks
// at once.
const authRecheck = 15 * time.Second

// authWatch ends open requests whose credential has been revoked, has
// expired or was signed out, so a stream opened before then does not keep
// receiving.
type authWatch struct {
	mu      sync.Mutex
	live    map[*watched]struct{}
	running bool
	kick    chan struct{}
}

type watched struct {
	ok     func(now time.Time) bool
	cancel context.CancelFunc
}

// watch returns a context for a request that is cancelled once ok fails,
// and a function to call when the request is done.
func (a *authWatch) watch(parent context.Context, ok func(time.Time) bool) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	w := &watched{ok: ok, cancel: cancel}
	a.mu.Lock()
	if a.live == nil {
		a.live = map[*watched]struct{}{}
		a.kick = make(chan struct{}, 1)
	}
	a.live[w] = struct{}{}
	if !a.running {
		a.running = true
		go a.loop()
	}
	a.mu.Unlock()
	return ctx, func() {
		a.mu.Lock()
		delete(a.live, w)
		a.mu.Unlock()
		cancel()
	}
}

// recheck checks every open request now (after a revocation or sign-out).
func (a *authWatch) recheck() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		select {
		case a.kick <- struct{}{}:
		default:
		}
	}
}

// loop runs while any request is open.
func (a *authWatch) loop() {
	t := time.NewTicker(authRecheck)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-a.kick:
		}
		a.mu.Lock()
		if len(a.live) == 0 {
			a.running = false
			a.mu.Unlock()
			return
		}
		ws := make([]*watched, 0, len(a.live))
		for w := range a.live {
			ws = append(ws, w)
		}
		a.mu.Unlock()
		now := time.Now()
		for _, w := range ws {
			if !w.ok(now) {
				w.cancel()
			}
		}
	}
}
