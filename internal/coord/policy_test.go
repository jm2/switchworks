// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/model"
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
	set := func(body string) int {
		r := httptest.NewRequest("POST", "/v1/sessions/"+sess.ID+"/policy", strings.NewReader(body))
		r.SetPathValue("id", sess.ID)
		w := httptest.NewRecorder()
		s.handlePolicy(w, r)
		return w.Code
	}
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
}
