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

// A policy change goes to the node, and the record takes the value asked
// for once the node has applied it; unknown values and refusals change
// nothing.
func TestHandlePolicy(t *testing.T) {
	s := testServer(t, Guards{})
	var got []wire.Cmd
	refuse := ""
	fakeNode(t, s, "devbox", func(cmd wire.Cmd) wire.Result {
		if cmd.Op == wire.OpSpawn {
			return wire.Result{OK: true, Data: model.Marshal(wire.SpawnResult{ApprovalPolicy: "on-request", Sandbox: "workspace-write"})}
		}
		got = append(got, cmd)
		if refuse != "" {
			return wire.Result{Error: refuse}
		}
		return wire.Result{OK: true}
	})
	sess, err := s.Spawn(context.Background(), model.SpawnSpec{Host: "devbox", Name: "sw", Cwd: "/srv/x"})
	if err != nil {
		t.Fatal(err)
	}
	tok := store.APIToken{ID: "01M58TOKEN0000000000000001", Name: "devbox-cli", Scope: "operate", Hash: "h2", Created: time.Now()}
	if err := s.store.AddAPIToken(tok); err != nil {
		t.Fatal(err)
	}
	asOwner := withGrant(context.Background(), grant{scope: scopeAdmin})
	asToken := withGrant(context.Background(), grant{scope: scopeOperate, tokenID: tok.ID})
	setAs := func(ctx context.Context, body string) int {
		r := httptest.NewRequest("POST", "/v1/sessions/"+sess.ID+"/policy", strings.NewReader(body)).WithContext(ctx)
		r.SetPathValue("id", sess.ID)
		w := httptest.NewRecorder()
		s.handlePolicy(w, r)
		return w.Code
	}
	set := func(body string) int { return setAs(asOwner, body) }
	policy := func() string {
		cur, _ := s.store.Session(sess.ID)
		return cur.ApprovalPolicy
	}

	if code := set(`{"approval_policy":"yolo"}`); code != 400 || len(got) != 0 {
		t.Fatalf("unknown policy: %d, sent %+v", code, got)
	}
	refuse = "agy sessions cannot change their approval policy while running"
	if code := set(`{"approval_policy":"never"}`); code != 400 || policy() != "on-request" {
		t.Fatalf("refused: %d, record %q", code, policy())
	}
	refuse = ""
	if code := set(`{"approval_policy":"never"}`); code != 200 {
		t.Fatalf("set: %d", code)
	}
	last := got[len(got)-1]
	if last.Op != wire.OpPolicy || last.Session != sess.ID || last.Policy == nil || last.Policy.ApprovalPolicy != "never" {
		t.Fatalf("command %+v", last)
	}
	cur, _ := s.store.Session(sess.ID)
	if cur.ApprovalPolicy != "never" || cur.PoliciesReported {
		t.Fatalf("record %q reported=%v", cur.ApprovalPolicy, cur.PoliciesReported)
	}

	// A token that is not the owner's may tighten, never loosen: an agent
	// that reads it on its host must not lift its own approvals.
	if code := setAs(asToken, `{"approval_policy":"untrusted"}`); code != 200 || policy() != "untrusted" {
		t.Fatalf("tighten with a token: %d, record %q", code, policy())
	}
	sent := len(got)
	for _, p := range []string{"on-request", "never"} {
		if code := setAs(asToken, `{"approval_policy":"`+p+`"}`); code != 403 {
			t.Fatalf("loosen to %s with a token: %d", p, code)
		}
	}
	if len(got) != sent || policy() != "untrusted" {
		t.Fatalf("a refused loosening reached the node or the record: %d commands, %q", len(got)-sent, policy())
	}
	if code := setAs(withGrant(context.Background(), grant{scope: scopeOperate, tokenID: tok.ID}), `{"approval_policy":"untrusted"}`); code != 200 {
		t.Fatalf("same policy with a token: %d", code)
	}
	if err := s.store.SetAPITokenOwner(tok.ID, true); err != nil {
		t.Fatal(err)
	}
	if code := setAs(asToken, `{"approval_policy":"on-request"}`); code != 200 || policy() != "on-request" {
		t.Fatalf("loosen with an owner token: %d, record %q", code, policy())
	}
}

// A session.policy event the record missed (a coordinator that stopped
// before saving the change) is projected, as the node's word.
func TestProjectPolicyEvent(t *testing.T) {
	s := testServer(t, Guards{})
	sess := addSession(t, s, "devbox", "sw", func(x *model.Session) { x.ApprovalPolicy = "untrusted" })
	ev := func(seq int64, p string) model.Event {
		return model.Event{SessionID: sess.ID, Seq: seq, Kind: model.KindSessionPolicy, TS: time.Now(),
			Payload: model.Marshal(model.SessionPolicy{ApprovalPolicy: p})}
	}
	s.project(ev(1, "never"))
	cur, _ := s.store.Session(sess.ID)
	if cur.ApprovalPolicy != "never" || !cur.PoliciesReported {
		t.Fatalf("after the event: %q reported=%v", cur.ApprovalPolicy, cur.PoliciesReported)
	}
	// One the record already has changes nothing, so the coordinator's own
	// (unreported) value stands.
	_, _ = s.store.UpdateSession(sess.ID, func(x *model.Session) { x.ApprovalPolicy, x.PoliciesReported = "on-request", false })
	s.project(ev(2, "on-request"))
	s.project(ev(3, "yolo"))
	cur, _ = s.store.Session(sess.ID)
	if cur.ApprovalPolicy != "on-request" || cur.PoliciesReported {
		t.Fatalf("after matching and bad events: %q reported=%v", cur.ApprovalPolicy, cur.PoliciesReported)
	}
}
