// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/wire"
)

// A person's message carries the credential it was sent with, set from
// the request: the admin token and owner tokens are the owner's, any other
// token is not, and nothing in the body can change that.
func TestMessageCredential(t *testing.T) {
	s := testServer(t, Guards{})
	delivered := make(chan *wire.MessageCmd, 8)
	var spawn *wire.SpawnCmd
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		switch cmd.Op {
		case wire.OpSpawn:
			spawn = cmd.Spawn
			return wire.Result{OK: true, Data: model.Marshal(wire.SpawnResult{ApprovalPolicy: "untrusted"})}
		case wire.OpDeliver:
			delivered <- cmd.Message
		}
		return wire.Result{OK: true, Data: model.Marshal(wire.DeliverResult{Mode: model.ModeQueue})}
	})
	tok := store.APIToken{ID: "01M58TOKEN0000000000000000", Name: "antec-cli", Scope: "operate", Hash: "h1", Created: time.Now()}
	if err := s.store.AddAPIToken(tok); err != nil {
		t.Fatal(err)
	}
	asAdmin := withGrant(context.Background(), grant{scope: scopeAdmin})
	asToken := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: tok.ID})

	// A spawn's first prompt: the credential travels outside the spec.
	sess, err := s.Spawn(asToken, model.SpawnSpec{Host: "devbox", Name: "sw", Cwd: "/srv/x", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if spawn.PromptVia != "token:antec-cli" || spawn.PromptOwner {
		t.Fatalf("spawn prompt credential %q owner=%v", spawn.PromptVia, spawn.PromptOwner)
	}

	send := func(ctx context.Context, body string) (int, *wire.MessageCmd) {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/sessions/"+sess.ID+"/messages", strings.NewReader(body)).WithContext(ctx)
		r.SetPathValue("id", sess.ID)
		w := httptest.NewRecorder()
		s.handleMessage(w, r)
		if w.Code != 202 {
			return w.Code, nil
		}
		select {
		case m := <-delivered:
			return w.Code, m
		case <-time.After(5 * time.Second):
			t.Fatal("not delivered")
			return 0, nil
		}
	}
	for _, c := range []struct {
		name  string
		ctx   context.Context
		via   string
		owner bool
	}{
		{"admin token", asAdmin, "admin", true},
		{"unmarked token", asToken, "token:antec-cli", false},
	} {
		code, m := send(c.ctx, `{"text":"ship it","from":"user:jm"}`)
		if code != 202 || m.From != "user:jm" || m.Via != c.via || m.Owner != c.owner {
			t.Fatalf("%s: %d %+v", c.name, code, m)
		}
	}
	// The body cannot claim a credential.
	if code, _ := send(asToken, `{"text":"ship it","owner":true,"via":"admin"}`); code != 400 {
		t.Fatalf("a body claiming a credential: %d", code)
	}

	// Marking the token takes effect on its next message.
	r := httptest.NewRequest("POST", "/v1/tokens/antec-cli/owner", strings.NewReader(`{"owner":true}`)).WithContext(asAdmin)
	r.SetPathValue("id", "antec-cli")
	w := httptest.NewRecorder()
	s.handleTokenOwner(w, r)
	if w.Code != 200 {
		t.Fatalf("owner: %d %s", w.Code, w.Body)
	}
	if _, m := send(asToken, `{"text":"ship it"}`); m.Via != "token:antec-cli" || !m.Owner {
		t.Fatalf("after marking: %+v", m)
	}
	r = httptest.NewRequest("POST", "/v1/tokens/nope/owner", strings.NewReader(`{"owner":true}`)).WithContext(asAdmin)
	r.SetPathValue("id", "nope")
	w = httptest.NewRecorder()
	s.handleTokenOwner(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown token: %d", w.Code)
	}
}
