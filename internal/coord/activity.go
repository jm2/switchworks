// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
)

// activity is what the fleet views show about a session beyond its record:
// its last agent message and its unanswered approval prompts. It lives in
// memory and is rebuilt from the tail of each log at startup.
type activity struct {
	preview   string
	last      string // the last agent message, clipped for fleet_status
	approvals map[string]model.ApprovalRequested
	order     []string // approval ids, oldest first
}

const (
	previewLen     = 280
	lastMessageLen = 4000
)

func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// track updates a session's activity from one event and reports whether
// the fleet views should hear about it.
func (s *Server) track(ev model.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acts[ev.SessionID]
	if a == nil {
		a = &activity{approvals: map[string]model.ApprovalRequested{}}
		s.acts[ev.SessionID] = a
	}
	switch ev.Kind {
	case model.KindItemCompleted:
		var it model.Item
		if json.Unmarshal(ev.Payload, &it) == nil && it.ItemType == "agentMessage" && it.Text != "" {
			a.preview = clipText(it.Text, previewLen)
			a.last = it.Text
			if len(a.last) > lastMessageLen {
				a.last = a.last[:lastMessageLen] + "…"
			}
			return true
		}
		return false
	case model.KindApprovalRequested:
		var p model.ApprovalRequested
		if json.Unmarshal(ev.Payload, &p) == nil {
			if _, dup := a.approvals[p.ApprovalID]; !dup {
				a.order = append(a.order, p.ApprovalID)
			}
			a.approvals[p.ApprovalID] = p
		}
	case model.KindApprovalResolved:
		var p model.ApprovalResolved
		if json.Unmarshal(ev.Payload, &p) == nil {
			a.drop(p.ApprovalID)
		}
	case model.KindTurnCompleted, model.KindSessionRegistered:
		// A finished turn (or a restarted harness) has no open prompts.
		a.approvals = map[string]model.ApprovalRequested{}
		a.order = nil
	case model.KindSessionStatus, model.KindUsage, model.KindTurnStarted, model.KindSessionSettled, model.KindSessionQueue:
	default:
		return false
	}
	return true
}

func (a *activity) drop(id string) {
	delete(a.approvals, id)
	for i, x := range a.order {
		if x == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// rebuildActivity replays the end of every live session's log.
func (s *Server) rebuildActivity() {
	for _, sess := range s.store.Sessions() {
		if !live(sess) {
			continue
		}
		after := s.store.LastSeq(sess.ID) - 400
		if after < 0 {
			after = 0
		}
		for {
			evs, err := s.store.Events(sess.ID, after, 400)
			if err != nil || len(evs) == 0 {
				break
			}
			for _, ev := range evs {
				s.track(ev)
				// Rebuild what started each session's current work, so a
				// restart does not hand running chains fresh hop counts.
				if ev.Kind == model.KindMessageDelivered {
					var p model.MessageEvent
					if json.Unmarshal(ev.Payload, &p) == nil {
						s.router.onDelivered(ev.SessionID, p.MessageID)
					}
				}
			}
			after = evs[len(evs)-1].Seq
		}
	}
}

// fillActivity adds activity to a session view.
func (s *Server) fillActivity(v *apiclient.SessionView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acts[v.ID]
	if a == nil {
		return
	}
	v.Preview = a.preview
	if v.Status == model.StatusStopped || v.Status == model.StatusGone {
		return
	}
	for _, id := range a.order {
		v.Approvals = append(v.Approvals, a.approvals[id])
	}
}

// --- the fleet stream ---

// FleetEvent is one event on GET /v1/stream.
type FleetEvent struct {
	Type string // "session", "session_removed", "host", "host_removed" or "message"
	Data any
}

func (s *Server) fleetPublish(typ string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.fleet {
		select {
		case ch <- FleetEvent{Type: typ, Data: data}:
		default:
			delete(s.fleet, ch)
			close(ch)
		}
	}
}

func (s *Server) publishSession(id string) {
	if sess, ok := s.store.Session(id); ok {
		s.fleetPublish("session", s.view(sess))
	}
}

func (s *Server) publishHost(id string) {
	if h, ok := s.store.Host(id); ok {
		h.Connected = s.Online(id)
		s.fleetPublish("host", h)
		// Session views say offline while their host is.
		for _, sess := range s.store.Sessions() {
			if sess.Host == id && live(sess) {
				s.fleetPublish("session", s.view(sess))
			}
		}
	}
}

// FleetHello is the first event on the fleet stream: everything a view
// needs to draw itself; later events update it.
type FleetHello struct {
	Hosts    []model.Host            `json:"hosts"`
	Sessions []apiclient.SessionView `json:"sessions"`
	Messages []model.Message         `json:"messages"` // the most recent, newest first
	Time     time.Time               `json:"time"`
}

func (s *Server) hello() FleetHello {
	h := FleetHello{Hosts: s.store.Hosts(), Sessions: []apiclient.SessionView{}, Time: time.Now().UTC()}
	for i := range h.Hosts {
		h.Hosts[i].Connected = s.Online(h.Hosts[i].ID)
	}
	for _, sess := range s.store.Sessions() {
		h.Sessions = append(h.Sessions, s.view(sess))
	}
	h.Messages = s.store.Messages(storeFilterRecent(200))
	if h.Messages == nil {
		h.Messages = []model.Message{}
	}
	return h
}

func (s *Server) handleFleetStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	ch := make(chan FleetEvent, 512)
	s.mu.Lock()
	s.fleet[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.fleet[ch]; ok {
			delete(s.fleet, ch)
			close(ch)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	send := func(typ string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		return err
	}
	// Subscribed first, so nothing between the snapshot and the stream is
	// lost (at worst an update arrives twice).
	if send("hello", s.hello()) != nil {
		return
	}
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // too slow; the client reconnects and gets a fresh hello
			}
			if send(ev.Type, ev.Data) != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func storeFilterRecent(n int) store.MessageFilter { return store.MessageFilter{Limit: n} }
