// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

// lastAgentMessage scans the end of a session's log for its last reply.
func (s *Server) lastAgentMessage(sessionID string) string {
	last := s.store.LastSeq(sessionID)
	after := last - 500
	if after < 0 {
		after = 0
	}
	evs, err := s.store.Events(sessionID, after, 0)
	if err != nil {
		return ""
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != model.KindItemCompleted {
			continue
		}
		var it model.Item
		if json.Unmarshal(evs[i].Payload, &it) == nil && it.ItemType == "agentMessage" && it.Text != "" {
			return it.Text
		}
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
	last := s.lastAgentMessage(sess.ID)
	if len(last) > 4000 {
		last = last[:4000] + "…"
	}
	return fleet.StatusResult{SessionInfo: s.info(sess), LastMessage: last, UpdatedAt: sess.UpdatedAt}, nil
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

func (s *Server) fleetSpawn(ctx context.Context, from sender, a fleet.SpawnArgs) (fleet.SpawnResult, error) {
	g := s.router.guards
	if from.sess != nil {
		if d := s.depth(*from.sess) + 1; d > g.MaxSpawnDepth {
			return fleet.SpawnResult{}, fmt.Errorf("spawn depth limit: this would be level %d (max %d)", d, g.MaxSpawnDepth)
		}
		if len(g.SpawnHosts) > 0 && !contains(g.SpawnHosts, a.Host) {
			return fleet.SpawnResult{}, fmt.Errorf("agents may not spawn on %q (allowed: %s)", a.Host, strings.Join(g.SpawnHosts, ", "))
		}
	}
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
	spec := model.SpawnSpec{Host: a.Host, Name: name, Harness: a.Harness, Cwd: a.Cwd, Prompt: a.Prompt,
		Model: a.Model, Labels: a.Labels, Parent: from.sessionID()}
	sess, err := s.Spawn(ctx, spec)
	if err != nil {
		return fleet.SpawnResult{}, err
	}
	s.router.started(sess.ID, s.router.hopsFor(from))
	return fleet.SpawnResult{ID: sess.ID, Path: sess.Path}, nil
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
