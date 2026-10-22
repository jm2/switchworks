// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/wire"
)

func sendRaw(t *testing.T, s *Server, sess model.Session, raw string) (fleet.SendResult, error) {
	t.Helper()
	res, err := s.fleetOp(context.Background(), from(sess), wire.ReqSend, json.RawMessage(raw))
	if err != nil {
		return fleet.SendResult{}, err
	}
	return res.(fleet.SendResult), nil
}

// One fleet_send to several addresses makes one message per recipient,
// sharing a group; addresses that cannot take it are refused on their own.
func TestSendToSeveral(t *testing.T) {
	s := testServer(t, Guards{})
	fakeNode(t, s, "devbox", func(wire.Cmd) wire.Result { return wire.Result{OK: true} })
	a := addSession(t, s, "devbox", "a")
	b := addSession(t, s, "devbox", "b")
	c := addSession(t, s, "devbox", "c")
	addSession(t, s, "devbox", "d")
	agy := addSession(t, s, "devbox", "agy", func(x *model.Session) { x.Harness = "agy" })

	res, err := sendRaw(t, s, a, `{"to": ["b", "devbox/c", "nobody", "devbox/a", "user:jm", "c"], "text": "heads up"}`)
	if err != nil {
		t.Fatal(err)
	}
	byTo := map[string]fleet.MessageInfo{}
	for _, m := range res.Messages {
		byTo[m.To] = m
	}
	if len(res.Messages) != 5 || res.Group == "" {
		t.Fatalf("result: %+v", res)
	}
	for _, to := range []string{"b", "devbox/c", "user:jm"} {
		if m := byTo[to]; m.ID == "" || m.State != model.MsgDelivered || m.Error != "" {
			t.Errorf("%s: %+v", to, m)
		}
	}
	if m := byTo["nobody"]; m.State != fleet.Refused || m.ID != "" || !strings.Contains(m.Error, "no live session") {
		t.Errorf("nobody: %+v", m)
	}
	if m := byTo["devbox/a"]; m.State != fleet.Refused || !strings.Contains(m.Error, "itself") {
		t.Errorf("self: %+v", m)
	}
	// c was named twice and got one message; each record has its own
	// delivery state and the address it was sent to.
	stored := 0
	for _, m := range s.store.Messages(store.MessageFilter{Session: a.ID}) {
		if m.Group != res.Group || m.Text != "heads up" {
			t.Errorf("record %+v", m)
		}
		stored++
	}
	if stored != 3 { // b, c and the person
		t.Fatalf("%d records, want 3", stored)
	}
	if m, _ := s.store.Message(byTo["devbox/c"].ID); m.ToSession != c.ID || m.To != "devbox/c" {
		t.Fatalf("c's record: %+v", m)
	}
	if m, _ := s.store.Message(byTo["b"].ID); m.ToSession != b.ID || m.To != "b" {
		t.Fatalf("b's record: %+v", m)
	}

	// Images: the session that cannot take them is refused alone.
	img := testPNG(t, 2)
	s.mu.Lock()
	s.nodes["devbox"].hello.Images = []string{"codex"}
	s.mu.Unlock()
	res, err = s.router.sendEach(context.Background(), from(a), model.MsgNotify,
		fleet.SendArgs{ToList: []string{"b", agy.Path}, Text: "look", Attachments: []model.Attachment{{Data: img}}})
	if err != nil || len(res.Messages) != 2 || res.Messages[0].State != model.MsgDelivered ||
		res.Messages[1].State != fleet.Refused || !strings.Contains(res.Messages[1].Error, "text only") {
		t.Fatalf("images: %+v %v", res, err)
	}

	// Nobody can take it: an error naming each.
	if _, err := sendRaw(t, s, a, `{"to": ["nobody", "a"], "text": "x"}`); err == nil ||
		!strings.Contains(err.Error(), "nobody:") || !strings.Contains(err.Error(), "a: a session cannot message itself") {
		t.Fatalf("all refused: %v", err)
	}
	// A list of one is a plain send.
	res, err = sendRaw(t, s, a, `{"to": ["b"], "text": "one"}`)
	if err != nil || len(res.Messages) != 1 || res.Group != "" {
		t.Fatalf("list of one: %+v %v", res, err)
	}
	// A broadcast selector in the list expands.
	res, err = sendRaw(t, s, a, `{"to": ["@harness=codex", "user:jm"], "broadcast": true, "text": "all"}`)
	if err != nil || len(res.Messages) != 4 { // b, c, d and jm
		t.Fatalf("broadcast in a list: %+v %v", res, err)
	}
	for _, bad := range []string{`{"to": [], "text": "x"}`, `{"to": [1], "text": "x"}`, `{"to": ["b"], "text": "x", "cc": "c"}`} {
		if _, err := sendRaw(t, s, a, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Limits count every recipient of a send to several.
func TestSendToSeveralLimits(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, Guards{MaxBroadcast: 2, SendPerMinute: 1})
	r := s.router
	a := addSession(t, s, "devbox", "a")
	addSession(t, s, "devbox", "b")
	addSession(t, s, "devbox", "c")
	addSession(t, s, "devbox", "d")

	if _, err := r.sendEach(ctx, from(a), model.MsgNotify, fleet.SendArgs{ToList: []string{"b", "c", "d"}, Text: "x"}); err == nil ||
		!strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("max_broadcast: %v", err)
	}
	if res, err := r.sendEach(ctx, from(a), model.MsgNotify, fleet.SendArgs{ToList: []string{"b", "c"}, Text: "x"}); err != nil || len(res.Messages) != 2 {
		t.Fatalf("first: %+v %v", res, err)
	}
	if _, err := r.sendEach(ctx, from(a), model.MsgNotify, fleet.SendArgs{ToList: []string{"c", "d"}, Text: "x"}); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate limit: %v", err)
	}
	// People are not limited, and the HTTP API takes a list too.
	w := httptest.NewRecorder()
	s.handlePostMessage(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"to": ["b", "c", "d", "e"], "text": "hi"}`)))
	var res fleet.SendResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 202 || len(res.Messages) != 4 || res.Group == "" ||
		res.Messages[3].State != fleet.Refused {
		t.Fatalf("API: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.handlePostMessage(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"to": ["b", "c"], "kind": "ask", "text": "?"}`)))
	if w.Code != 400 {
		t.Fatalf("ask to several: %d %s", w.Code, w.Body)
	}
}
