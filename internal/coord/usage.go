// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"encoding/json"
	"sync"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
)

// usageBook keeps, per session, the Total of the last usage event its
// adapter reported, so a session's usage (and the daily token count) grows
// by what each event adds even when the adapter's Total starts over.
//
// Adapters differ in what their Total spans. Codex's covers the thread and
// survives resumes (thread/tokenUsage/updated also repeats an unchanged
// figure). acpad and agyad count from their agent process, so their Total
// restarts at zero whenever the session is reopened. Summing Last would
// double count Codex's repeats, and taking Total as the session's usage
// loses everything before a restart; adding the growth of Total, and the
// whole Total when it started over, is right for all of them.
type usageBook struct {
	mu   sync.Mutex
	last map[string]model.Usage // session id -> adapter's last Total
}

// prev returns the adapter Total reported before the usage event at seq,
// from memory or, after a coordinator restart, from the session's log.
// It must not be called while the store is locked (in UpdateSession).
func (b *usageBook) prev(st store.Store, sessionID string, seq int64) (model.Usage, bool) {
	b.mu.Lock()
	u, ok := b.last[sessionID]
	b.mu.Unlock()
	if ok {
		return u, true
	}
	var after int64
	for after < seq-1 {
		evs, err := st.Events(sessionID, after, 0)
		if err != nil || len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			if ev.Seq >= seq {
				break
			}
			after = ev.Seq
			if ev.Kind != model.KindUsage {
				continue
			}
			var p model.UsageEvent
			if json.Unmarshal(ev.Payload, &p) == nil {
				u, ok = p.Total, true
			}
		}
		if evs[len(evs)-1].Seq >= seq {
			break
		}
	}
	return u, ok
}

func (b *usageBook) forget(sessionID string) {
	b.mu.Lock()
	delete(b.last, sessionID)
	b.mu.Unlock()
}

func (b *usageBook) record(sessionID string, total model.Usage) {
	b.mu.Lock()
	if b.last == nil {
		b.last = map[string]model.Usage{}
	}
	b.last[sessionID] = total
	b.mu.Unlock()
}

// usageAdded is what a usage event adds to its session, given the
// adapter's previous Total (known says there was one). A Total that grew
// in every field adds its growth; one that shrank, or that equals the
// event's Last (the first report of a new agent process), started over and
// adds all of itself.
func usageAdded(prev model.Usage, known bool, ev model.UsageEvent) model.Usage {
	cur := ev.Total
	switch {
	case known && cur == prev:
		return model.Usage{} // a repeat
	case known && usageCovers(cur, prev) && (cur != ev.Last || prev == model.Usage{}):
		return model.Usage{
			InputTokens: cur.InputTokens - prev.InputTokens, CachedInputTokens: cur.CachedInputTokens - prev.CachedInputTokens,
			OutputTokens: cur.OutputTokens - prev.OutputTokens, ReasoningTokens: cur.ReasoningTokens - prev.ReasoningTokens,
			TotalTokens: cur.TotalTokens - prev.TotalTokens,
		}
	}
	// Never negative, even from an adapter that reported so.
	cur.InputTokens = max(cur.InputTokens, 0)
	cur.CachedInputTokens = max(cur.CachedInputTokens, 0)
	cur.OutputTokens = max(cur.OutputTokens, 0)
	cur.ReasoningTokens = max(cur.ReasoningTokens, 0)
	cur.TotalTokens = max(cur.TotalTokens, 0)
	return cur
}

func usageCovers(a, b model.Usage) bool {
	return a.InputTokens >= b.InputTokens && a.CachedInputTokens >= b.CachedInputTokens &&
		a.OutputTokens >= b.OutputTokens && a.ReasoningTokens >= b.ReasoningTokens &&
		a.TotalTokens >= b.TotalTokens
}

func addUsageTo(dst *model.Usage, u model.Usage) {
	dst.InputTokens += u.InputTokens
	dst.CachedInputTokens += u.CachedInputTokens
	dst.OutputTokens += u.OutputTokens
	dst.ReasoningTokens += u.ReasoningTokens
	dst.TotalTokens += u.TotalTokens
}
