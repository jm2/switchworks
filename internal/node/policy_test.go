// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// policyAdapter starts handles that can change their approval policy when
// settable is true.
type policyAdapter struct {
	settable bool
	h        *policyHandle
}

func (a *policyAdapter) Name() string { return "fake" }
func (a *policyAdapter) Start(_ context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	a.h = &policyHandle{policy: spec.ApprovalPolicy, settable: a.settable}
	sink(adapter.Event{Kind: model.KindSessionRegistered, Payload: model.SessionRegistered{NativeRef: a.h.Ref(), Caps: a.h.Caps()}})
	if !a.settable {
		return plainHandle{a.h}, nil
	}
	return a.h, nil
}
func (a *policyAdapter) Resume(context.Context, model.NativeRef, adapter.StartSpec, adapter.Sink) (adapter.Handle, error) {
	return nil, os.ErrNotExist
}
func (a *policyAdapter) Close() error { return nil }

type policyHandle struct {
	policy   string
	settable bool
}

func (h *policyHandle) Ref() model.NativeRef { return model.NativeRef{Harness: "fake", ID: "t1"} }
func (h *policyHandle) Caps() model.Caps     { return model.Caps{Message: true, Policy: h.settable} }
func (h *policyHandle) Status() string       { return model.StatusIdle }
func (h *policyHandle) Deliver(context.Context, adapter.Input, adapter.DeliveryMode) error {
	return nil
}
func (h *policyHandle) Interrupt(context.Context) error               { return nil }
func (h *policyHandle) Resolve(context.Context, string, string) error { return nil }
func (h *policyHandle) Stop(context.Context) error                    { return nil }
func (h *policyHandle) SetApprovalPolicy(_ context.Context, p string) error {
	h.policy = p
	return nil
}

// plainHandle hides SetApprovalPolicy.
type plainHandle struct{ h *policyHandle }

func (p plainHandle) Ref() model.NativeRef { return p.h.Ref() }
func (p plainHandle) Caps() model.Caps     { return p.h.Caps() }
func (p plainHandle) Status() string       { return p.h.Status() }
func (p plainHandle) Deliver(ctx context.Context, in adapter.Input, m adapter.DeliveryMode) error {
	return p.h.Deliver(ctx, in, m)
}
func (p plainHandle) Interrupt(ctx context.Context) error { return p.h.Interrupt(ctx) }
func (p plainHandle) Resolve(ctx context.Context, a, d string) error {
	return p.h.Resolve(ctx, a, d)
}
func (p plainHandle) Stop(ctx context.Context) error { return p.h.Stop(ctx) }

func policyNode(t *testing.T, a *policyAdapter) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{dir: dir, logf: t.Logf, adapters: map[string]adapter.Adapter{"fake": a}, sessions: map[string]*session{}}
	id := ulid.New()
	_, err := n.spawn(context.Background(), &wire.SpawnCmd{Session: model.Session{ID: id, Path: "devbox/a"},
		Spec: model.SpawnSpec{Harness: "fake", Cwd: t.TempDir(), ApprovalPolicy: "untrusted"}})
	if err != nil {
		t.Fatal(err)
	}
	return n, id
}

// A policy change reaches the harness, is kept for a resume and is logged.
func TestSetPolicy(t *testing.T) {
	a := &policyAdapter{settable: true}
	n, id := policyNode(t, a)
	res := n.handleCmd(wire.Cmd{Op: wire.OpPolicy, Session: id, Policy: &wire.PolicyCmd{ApprovalPolicy: "never"}})
	if !res.OK {
		t.Fatalf("policy: %s", res.Error)
	}
	if a.h.policy != "never" {
		t.Fatalf("harness has %q", a.h.policy)
	}
	s, _ := n.session(id)
	s.mu.Lock()
	kept, evs := s.rec.Spec.ApprovalPolicy, append([]model.Event(nil), s.unacked...)
	s.mu.Unlock()
	if kept != "never" {
		t.Fatalf("record keeps %q", kept)
	}
	if b, _ := os.ReadFile(filepath.Join(n.dir, "state.json")); !strings.Contains(string(b), `"ApprovalPolicy": "never"`) {
		t.Fatalf("state.json does not keep the policy: %s", b)
	}
	if last := evs[len(evs)-1]; last.Kind != model.KindSessionPolicy || !strings.Contains(string(last.Payload), `"never"`) {
		t.Fatalf("last event %s %s", last.Kind, last.Payload)
	}
}

// A harness that cannot change its policy refuses, and nothing changes.
func TestSetPolicyUnsupported(t *testing.T) {
	a := &policyAdapter{}
	n, id := policyNode(t, a)
	res := n.handleCmd(wire.Cmd{Op: wire.OpPolicy, Session: id, Policy: &wire.PolicyCmd{ApprovalPolicy: "never"}})
	if res.OK || !strings.Contains(res.Error, "cannot change") {
		t.Fatalf("policy: %+v", res)
	}
	s, _ := n.session(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.Spec.ApprovalPolicy != "untrusted" || a.h.policy != "untrusted" {
		t.Fatalf("policy changed to %q / %q", s.rec.Spec.ApprovalPolicy, a.h.policy)
	}
}
