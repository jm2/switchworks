// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jm2/switchyard/internal/attach"
	"github.com/jm2/switchyard/internal/fleet"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/wire"
)

// handleNodeReq runs a fleet operation on behalf of one of a node's
// sessions and answers it.
func (s *Server) handleNodeReq(n *nodeConn, env wire.Envelope) {
	answer := func(data any, err error) {
		r := wire.Result{OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		} else if data != nil {
			b, _ := json.Marshal(data)
			r.Data = b
		}
		_ = n.conn.Send(wire.TResult, "", env.ID, r)
	}
	req, err := wire.Decode[wire.NodeReq](env)
	if err != nil {
		answer(nil, fmt.Errorf("bad request: %w", err))
		return
	}
	caller, ok := s.store.Session(req.Session)
	if !ok || caller.Host != n.host {
		answer(nil, fmt.Errorf("session %s is not on %s", req.Session, n.host))
		return
	}
	if !live(caller) {
		answer(nil, errors.New("calling session is stopped"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-n.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	answer(s.fleetOp(ctx, sender{sess: &caller}, req.Op, req.Args))
}

func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("bad arguments: %w", err)
	}
	return v, nil
}

// fleetOp dispatches one operation for a sender.
func (s *Server) fleetOp(ctx context.Context, from sender, op string, raw json.RawMessage) (any, error) {
	switch op {
	case wire.ReqList:
		a, err := decodeArgs[fleet.ListArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.fleetList(from, a), nil
	case wire.ReqSend:
		a, err := decodeArgs[fleet.SendArgs](raw)
		if err != nil {
			return nil, err
		}
		kind := model.MsgNotify
		if a.Broadcast {
			kind = model.MsgBroadcast
		}
		msgs, err := s.router.send(ctx, from, kind, a)
		if err != nil {
			return nil, err
		}
		return fleet.SendResult{Messages: infos(msgs)}, nil
	case wire.ReqAsk:
		a, err := decodeArgs[fleet.AskArgs](raw)
		if err != nil {
			return nil, err
		}
		res := s.router.ask(ctx, from, a)
		if res.Status == fleet.AskFailed && res.MessageID == "" {
			return nil, errors.New(res.Error)
		}
		return res, nil
	case wire.ReqReply:
		a, err := decodeArgs[fleet.ReplyArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.router.reply(ctx, from, a)
	case wire.ReqInbox:
		a, err := decodeArgs[fleet.InboxArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.router.inbox(ctx, from, a)
	case wire.ReqSpawn:
		a, err := decodeArgs[fleet.SpawnArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.fleetSpawn(ctx, from, a)
	case wire.ReqStatus:
		a, err := decodeArgs[fleet.StatusArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.fleetStatus(from, a)
	case wire.ReqStop:
		a, err := decodeArgs[fleet.StopArgs](raw)
		if err != nil {
			return nil, err
		}
		return s.fleetStop(ctx, from, a)
	}
	return nil, fmt.Errorf("unknown operation %q", op)
}

func infos(msgs []model.Message) []fleet.MessageInfo {
	out := make([]fleet.MessageInfo, len(msgs))
	for i, m := range msgs {
		out[i] = fleet.MessageInfo{ID: m.ID, To: m.To, State: m.State, Mode: m.Mode, Error: m.Error, Target: m.ToPath}
	}
	return out
}

func (s *Server) info(sess model.Session) fleet.SessionInfo {
	v := s.view(sess)
	return fleet.SessionInfo{ID: sess.ID, Path: sess.Path, Harness: sess.Harness, Status: v.Status, Online: v.Online,
		Labels: sess.Labels, Parent: sess.Parent, Model: sess.Model, Cwd: sess.Cwd, Usage: sess.Usage, Caps: sess.Caps}
}

func (s *Server) fleetList(from sender, a fleet.ListArgs) fleet.ListResult {
	res := fleet.ListResult{Self: from.addr(), Hosts: []fleet.HostInfo{}, Sessions: []fleet.SessionInfo{}}
	for _, h := range s.store.Hosts() {
		if a.Host != "" && h.ID != a.Host {
			continue
		}
		hs := h.Adapters
		if hs == nil {
			hs = []string{}
		}
		res.Hosts = append(res.Hosts, fleet.HostInfo{Host: h.ID, Online: s.Online(h.ID), Harnesses: hs})
	}
	var want map[string]string
	if a.Label != "" {
		k, v, _ := strings.Cut(a.Label, "=")
		want = map[string]string{k: v}
	}
	for _, sess := range s.store.Sessions() {
		if !a.All && !live(sess) {
			continue
		}
		if a.Host != "" && sess.Host != a.Host {
			continue
		}
		if want != nil && !selectorMatch(sess, want) {
			continue
		}
		res.Sessions = append(res.Sessions, s.info(sess))
	}
	return res
}

// lastAgentMessage is a session's last reply, as the activity tracker
// keeps it (clipped), without reading its log.
func (s *Server) lastAgentMessage(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.acts[sessionID]; a != nil {
		return a.last
	}
	return ""
}

func (s *Server) fleetStatus(from sender, a fleet.StatusArgs) (fleet.StatusResult, error) {
	ref := a.Session
	if ref == "" || ref == "self" {
		if from.sess == nil {
			return fleet.StatusResult{}, errors.New("session is required")
		}
		ref = from.sess.ID
	}
	var sess model.Session
	if ref == "parent" {
		t, err := s.router.resolve(from, "parent", false)
		if err != nil {
			return fleet.StatusResult{}, err
		}
		sess = *t[0].sess
	} else {
		var err error
		if sess, err = s.router.lookup(ref); err != nil {
			return fleet.StatusResult{}, err
		}
	}
	res := fleet.StatusResult{SessionInfo: s.info(sess), UpdatedAt: sess.UpdatedAt}
	// Agents read the output of themselves, their parent and their children
	// only, unless the guards allow more.
	if from.sess == nil || s.router.guards.AllowReadAny || sess.ID == from.sess.ID ||
		sess.ID == from.sess.Parent || sess.Parent == from.sess.ID {
		res.LastMessage = s.lastAgentMessage(sess.ID)
	}
	return res, nil
}

// depth is how many spawn levels sit above a session.
func (s *Server) depth(sess model.Session) int {
	d := 0
	for sess.Parent != "" && d < 32 {
		p, ok := s.store.Session(sess.Parent)
		if !ok {
			break
		}
		sess = p
		d++
	}
	return d
}

// Label keys agents may not set on sessions they spawn, so they cannot
// claim routes the owner set up (@role=reviewer, say). Keys starting with
// "owner." are reserved too.
var reservedLabels = map[string]bool{"host": true, "harness": true, "parent": true}

func (s *Server) fleetSpawn(ctx context.Context, from sender, a fleet.SpawnArgs) (fleet.SpawnResult, error) {
	g := s.router.guards
	if g.MaxDailyTokens > 0 && s.store.DailyTokens(time.Now().UTC().Format("2006-01-02")) >= g.MaxDailyTokens {
		return fleet.SpawnResult{}, ErrBudget
	}
	name := a.Name
	if name == "" {
		base := "child"
		if from.sess != nil {
			base = from.sess.Name
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[i+1:]
			}
		}
		id := strings.ToLower(ulid.New())
		name = fmt.Sprintf("%s-%s", base, id[len(id)-6:])
	}
	if len(a.Images) > 0 {
		return fleet.SpawnResult{}, errors.New("image paths cannot be read: the sending host runs an older swyd node; upgrade it")
	}
	spec := model.SpawnSpec{Host: a.Host, Name: name, Harness: a.Harness, Cwd: a.Cwd, Prompt: a.Prompt,
		Attachments: a.Attachments, Model: a.Model, Labels: a.Labels, Parent: from.sessionID()}
	hops := s.router.hopsFor(from)
	if from.sess != nil {
		if err := s.agentSpawn(from, &spec, hops); err != nil {
			return fleet.SpawnResult{}, err
		}
		if len(a.Attachments) > 0 {
			if strings.TrimSpace(a.Prompt) == "" {
				return fleet.SpawnResult{}, errors.New("images need a prompt to go with them")
			}
			// Refuse before starting a child that could not see them.
			if err := s.imagesRefusal(spec.Host, spec.Harness, nil); err != nil {
				return fleet.SpawnResult{}, err
			}
			if _, err := attach.Normalize(a.Attachments); err != nil {
				return fleet.SpawnResult{}, err
			}
		}
	}
	sess, err := s.Spawn(ctx, spec)
	if err != nil {
		return fleet.SpawnResult{}, err
	}
	s.router.started(sess.ID, hops)
	if from.sess != nil && strings.TrimSpace(a.Prompt) != "" {
		// An agent's prompt goes to the child as a routed message from the
		// agent: it carries the hop count, and the child sees who sent it
		// rather than taking it for the owner's words.
		msgs, err := s.router.send(ctx, from, model.MsgNotify, fleet.SendArgs{To: sess.ID, Text: a.Prompt, Attachments: a.Attachments})
		if err == nil && len(a.Attachments) > 0 && len(msgs) == 1 && msgs[0].State == model.MsgFailed {
			err = errors.New(msgs[0].Error) // e.g. the child's agent does not take images
		}
		if err != nil {
			// Stop the child rather than leave it idle with no task.
			_, _ = s.SessionCommand(ctx, sess.ID, wire.Cmd{Op: wire.OpStop})
			return fleet.SpawnResult{}, fmt.Errorf("spawned %s, but its prompt could not be sent (%w), so it was stopped", sess.Path, err)
		}
	}
	return fleet.SpawnResult{ID: sess.ID, Path: sess.Path}, nil
}

// agentSpawn applies the guards to a spawn by an agent and confines the
// child to its parent's scope: same host, harness and directory tree unless
// the guards allow more, and the parent's approval policy and sandbox.
func (s *Server) agentSpawn(from sender, spec *model.SpawnSpec, hops int) error {
	g := s.router.guards
	p := *from.sess
	if d := s.depth(p) + 1; d > g.MaxSpawnDepth {
		return fmt.Errorf("spawn depth limit: this would be level %d (max %d)", d, g.MaxSpawnDepth)
	}
	if hops > g.MaxHops {
		return ErrHopLimit
	}
	if spec.Host == "" {
		spec.Host = p.Host
	}
	switch {
	case spec.Host == p.Host, contains(g.SpawnHosts, "*"), contains(g.SpawnHosts, spec.Host):
	case len(g.SpawnHosts) == 0:
		return fmt.Errorf("agents may spawn only on their own host (%s); guards.spawn_hosts allows others", p.Host)
	default:
		return fmt.Errorf("agents may not spawn on %q (allowed: %s, %s)", spec.Host, p.Host, strings.Join(g.SpawnHosts, ", "))
	}
	if spec.Harness == "" {
		spec.Harness = p.Harness
	}
	if spec.Harness != p.Harness && !contains(g.SpawnHarnesses, "*") && !contains(g.SpawnHarnesses, spec.Harness) {
		return fmt.Errorf("agents may spawn only their own harness (%s); guards.spawn_harnesses allows others", p.Harness)
	}
	if spec.Cwd == "" {
		spec.Cwd = p.Cwd
	}
	// On another host the parent's cwd means nothing (and the node that
	// reported it may not be trusted there): only the configured roots.
	parentRoot := p.Cwd
	if spec.Host != p.Host {
		parentRoot = ""
	}
	root, ok := cwdRoot(parentRoot, spec.Cwd, g.SpawnCwdRoots)
	if !ok {
		if parentRoot == "" {
			return fmt.Errorf("cwd %q on another host must be inside guards.spawn_cwd_roots", spec.Cwd)
		}
		return fmt.Errorf("cwd %q is outside %s; guards.spawn_cwd_roots allows other directories", spec.Cwd, p.Cwd)
	}
	spec.CwdRoot = root
	for k := range spec.Labels {
		if reservedLabels[k] || strings.HasPrefix(k, "owner.") {
			return fmt.Errorf("label %q is reserved for people", k)
		}
	}
	// A sandboxed agent may not start an unsandboxed one: a harness that
	// ignores the sandbox setting (ACP agents) would get the user's full
	// access, whatever the parent was limited to.
	if s.enforcesSandbox(p.Host, p.Harness) && !s.enforcesSandbox(spec.Host, spec.Harness) &&
		p.Sandbox != "danger-full-access" && !g.AllowUnsandboxedSpawn {
		return fmt.Errorf("%s does not enforce a sandbox, and this session is sandboxed (%s); guards.allow_unsandboxed_spawn allows it",
			spec.Harness, orDefault(p.Sandbox, "unknown"))
	}
	// Likewise for approvals: headless agy never asks, so a child there
	// would run everything a person approves for its parent unasked.
	if spec.Harness == "agy" && p.Harness != "agy" && p.ApprovalPolicy != "never" && !g.AllowUnapprovedSpawn {
		return fmt.Errorf("agy does not ask for approvals, and this session's policy is %q; guards.allow_unapproved_spawn allows it",
			orDefault(p.ApprovalPolicy, "unknown"))
	}
	spec.ApprovalPolicy, spec.Sandbox = p.ApprovalPolicy, p.Sandbox
	if spec.Host != p.Host && p.PoliciesReported {
		// Policies a node reported for its own sessions are not carried to
		// another host.
		spec.ApprovalPolicy, spec.Sandbox = "", ""
	}
	if spec.ApprovalPolicy == "" {
		spec.ApprovalPolicy = "untrusted" // unknown: the strictest, not the node's default
	}
	if spec.Sandbox == "" {
		spec.Sandbox = "read-only"
	}
	spec.Prompt, spec.Attachments = "", nil // sent as a message once the child exists
	if !s.router.allow("spawn|"+p.ID, g.SpawnPerMinute) {
		return fmt.Errorf("spawn rate limit: at most %d a minute", g.SpawnPerMinute)
	}
	return nil
}

// enforcesSandbox reports whether harness on host applies the sandbox
// setting, as the host's node says (older nodes: Codex and agy do).
func (s *Server) enforcesSandbox(host, harness string) bool {
	if n, ok := s.node(host); ok && n.hello.Sandboxed != nil {
		return contains(n.hello.Sandboxed, harness)
	}
	return harness == "codex" || harness == "agy" || harness == ""
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// cwdRoot returns the directory an agent's spawn cwd lies inside: its own
// cwd, or one of the configured roots. The node checks the result again
// with symlinks resolved.
func cwdRoot(parentCwd, cwd string, roots []string) (string, bool) {
	if contains(roots, "*") {
		return "", true
	}
	for _, r := range append([]string{parentCwd}, roots...) {
		if r != "" && pathWithin(r, cwd) {
			return r, true
		}
	}
	return "", false
}

// pathWithin reports whether dir is root or below it, comparing absolute
// paths as text (in either separator style; a Windows path's case is
// ignored). Paths with ".." segments never match.
func pathWithin(root, dir string) bool {
	norm := func(p string) (string, bool) {
		p = strings.ReplaceAll(p, "\\", "/")
		win := len(p) >= 3 && p[1] == ':' && p[2] == '/'
		if !win && !strings.HasPrefix(p, "/") {
			return "", false
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return "", false
			}
		}
		p = strings.TrimRight(path.Clean(p), "/")
		if win {
			p = strings.ToLower(p)
		}
		return p, true
	}
	r, ok1 := norm(root)
	d, ok2 := norm(dir)
	if !ok1 || !ok2 {
		return false
	}
	return d == r || strings.HasPrefix(d, r+"/") || r == ""
}

func (s *Server) fleetStop(ctx context.Context, from sender, a fleet.StopArgs) (fleet.StopResult, error) {
	sess, err := s.router.lookup(a.Session)
	if err != nil {
		return fleet.StopResult{}, err
	}
	if from.sess != nil && sess.ID != from.sess.ID && sess.Parent != from.sess.ID && !s.router.guards.AllowStopAny {
		return fleet.StopResult{}, errors.New("agents may only stop sessions they spawned (or themselves)")
	}
	if _, err := s.SessionCommand(ctx, sess.ID, wire.Cmd{Op: wire.OpStop}); err != nil {
		return fleet.StopResult{}, err
	}
	return fleet.StopResult{Path: sess.Path, Status: model.StatusStopped}, nil
}
