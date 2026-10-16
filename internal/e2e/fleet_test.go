// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/coord"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/node"
)

// enrollHost enrolls a new host and starts its node.
func enrollHost(t *testing.T, c *apiclient.Client, host string) (dir, fakeState string, n *runningNode) {
	t.Helper()
	tok, err := c.EnrollToken(context.Background(), host, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if _, err := node.Enroll(context.Background(), dir, tok, false); err != nil {
		t.Fatal(err)
	}
	fakeState = t.TempDir()
	return dir, fakeState, startNode(t, dir, fakeState)
}

func agentSaidAll(subs ...string) func(model.Event) bool {
	return func(e model.Event) bool {
		if !agentSaid("")(e) {
			return false
		}
		var it model.Item
		_ = json.Unmarshal(e.Payload, &it)
		for _, s := range subs {
			if !strings.Contains(it.Text, s) {
				return false
			}
		}
		return true
	}
}

func lastAgentText(w *watcher, from int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.evs) - 1; i >= from; i-- {
		if agentSaid("")(w.evs[i]) {
			var it model.Item
			_ = json.Unmarshal(w.evs[i].Payload, &it)
			return it.Text
		}
	}
	return ""
}

func call(tool string, args any) string {
	b, _ := json.Marshal(args)
	return fmt.Sprintf("CALL %s %s", tool, b)
}

func countAgent(w *watcher, pred func(model.Event) bool) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, e := range w.evs {
		if pred(e) {
			n++
		}
	}
	return n
}

// TestFleetMessaging drives agent-to-agent traffic across two hosts: the
// fake app-server calls the real `swyd mcp` (this test binary) through the
// MCP config the node injects, which calls the node's local API, which
// calls the coordinator's router.
func TestFleetMessaging(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	work := t.TempDir()
	// Let agents spawn on other hosts (by default only on their own).
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0", func(c *coord.Config) {
		c.Guards.SpawnHosts = []string{"*"}
		c.Guards.SpawnCwdRoots = []string{work} // on another host only configured roots count
	})
	defer co.stop()
	c := apiClient(t, coordDir)

	_, _, ndA := enrollHost(t, c, "devbox")
	dirB, stateB, ndB := enrollHost(t, c, "buildbox")
	defer func() { ndA.stop(t) }()
	waitOnline(t, c, "devbox")
	waitOnline(t, c, "buildbox")

	lead, err := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "lead", Cwd: work, Labels: map[string]string{"role": "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	helper, err := c.Spawn(ctx, model.SpawnSpec{Host: "buildbox", Name: "helper", Cwd: work, Labels: map[string]string{"role": "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	wl := watch(c, lead.ID)
	defer wl.cancel()
	wh := watch(c, helper.ID)
	defer wh.cancel()
	waitStatus(t, c, lead.ID, model.StatusIdle)
	waitStatus(t, c, helper.ID, model.StatusIdle)
	say := func(sess apiclient.SessionView, text string) {
		t.Helper()
		if _, err := c.Send(ctx, sess.ID, text, "queue", ""); err != nil {
			t.Fatal(err)
		}
	}

	// fleet_list sees both hosts.
	say(lead, call("fleet_list", map[string]any{}))
	i := wl.waitFor(t, "fleet_list", 0, agentSaidAll("tool fleet_list:", "buildbox/helper", `"self": "devbox/lead"`))

	// A cross-host ask, answered while the asker waits: the answer comes
	// back inline, and is not also delivered as a turn.
	say(lead, call("fleet_ask", map[string]any{"to": "buildbox/helper", "text": "what is 6x7?", "wait_seconds": 30}))
	j := wh.waitFor(t, "helper replied", 0, agentSaidAll("replied to devbox/lead"))
	i = wl.waitFor(t, "inline answer", i, agentSaidAll("tool fleet_ask: Answer from buildbox/helper:", "answer: what is 6x7?"))
	wl.waitFor(t, "ask turn done", i, turnEnded("completed"))
	if txt := lastAgentText(wh, j); !strings.Contains(txt, `"inline": true`) && !strings.Contains(txt, `"state": "held"`) {
		t.Fatalf("reply was neither inline nor held for the asker: %s", txt)
	}

	// An ask by @label with no wait: the answer arrives later as a turn.
	say(lead, call("fleet_ask", map[string]any{"to": "@role=helper", "text": "status?", "wait_seconds": 0}))
	i = wl.waitFor(t, "ticket", i, agentSaidAll("tool fleet_ask:", `"status": "pending"`))
	i = wl.waitFor(t, "late answer as a turn", i, agentSaidAll("kind=reply", "answer: status?"))
	waitStatus(t, c, lead.ID, model.StatusIdle)
	time.Sleep(300 * time.Millisecond)
	if n := countAgent(wl, agentSaidAll("kind=reply", "answer: what is 6x7?")); n != 0 {
		t.Fatalf("the inline answer was also delivered as a turn (%d)", n)
	}

	// Messages to people land in the inbox; a person answers an agent's ask.
	say(lead, call("fleet_ask", map[string]any{"to": "user:jm", "text": "ship it?", "wait_seconds": 0}))
	i = wl.waitFor(t, "ask to a person", i, agentSaidAll(`"status": "pending"`, `"target": "user:jm"`))
	inbox, err := c.Messages(ctx, apiclient.MessageQuery{Inbox: true})
	if err != nil || len(inbox) != 1 || inbox[0].Kind != model.MsgAsk || inbox[0].From != "devbox/lead" {
		t.Fatalf("inbox: %+v %v", inbox, err)
	}
	if _, err := c.Reply(ctx, inbox[0].ID, "yes, ship it", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reply(ctx, inbox[0].ID, "again", ""); err == nil {
		t.Fatal("an ask was answered twice")
	}
	i = wl.waitFor(t, "person's answer", i, agentSaidAll("kind=reply", "from=user:cli", "yes, ship it"))

	// A person asks an agent and waits for the answer.
	// The helper answers through a fresh `swyd mcp` process, which can be
	// slow under the race detector on a busy CI runner.
	wait := 60
	ar, err := c.Ask(ctx, apiclient.PostMessage{To: "helper", Text: "your name?", WaitSeconds: &wait})
	if err != nil || ar.Status != "replied" || ar.Reply != "answer: your name?" || ar.ReplyFrom != "buildbox/helper" {
		m, _ := c.Message(ctx, ar.MessageID)
		t.Fatalf("person's ask: %+v %v\nask record: %+v\nhelper's last reply: %q", ar, err, m, lastAgentText(wh, j))
	}

	// Guards and errors come back to the agent as tool errors.
	say(lead, call("fleet_send", map[string]any{"to": "devbox/lead", "text": "me"}))
	i = wl.waitFor(t, "self-send refused", i, agentSaidAll("tool fleet_send: Error:", "cannot message itself"))
	say(lead, call("fleet_send", map[string]any{"to": "nobody", "text": "hi"}))
	i = wl.waitFor(t, "unknown target", i, agentSaidAll("tool fleet_send: Error:", `no live session named "nobody"`))
	say(lead, call("fleet_stop", map[string]any{"session": "buildbox/helper"}))
	i = wl.waitFor(t, "stop refused", i, agentSaidAll("tool fleet_stop: Error:", "only stop sessions they spawned"))

	// Spawn: the child runs, finishes, and its parent hears about it.
	say(lead, call("fleet_spawn", map[string]any{"host": "buildbox", "cwd": work, "prompt": "build it", "name": "worker"}))
	i = wl.waitFor(t, "spawned", i, agentSaidAll("tool fleet_spawn:", `"path": "buildbox/worker"`))
	// The prompt reaches the child as a message from its parent.
	i = wl.waitFor(t, "completion notice", i, agentSaidAll("kind=done", "buildbox/worker", "from=devbox/lead kind=notify", "build it"))
	worker, err := c.Resolve(ctx, "buildbox/worker")
	if err != nil || worker.Parent != lead.ID {
		t.Fatalf("worker: %+v %v", worker.Session, err)
	}
	say(lead, call("fleet_status", map[string]any{"session": "worker"}))
	i = wl.waitFor(t, "status", i, agentSaidAll("tool fleet_status:", `"last_message": "echo: [`, "build it"))
	say(lead, call("fleet_stop", map[string]any{"session": "buildbox/worker"}))
	i = wl.waitFor(t, "stopped child", i, agentSaidAll("tool fleet_stop:", `"status": "stopped"`))
	waitStatus(t, c, worker.ID, model.StatusStopped)

	// An agent message to an offline host waits and arrives exactly once.
	ndB.stop(t)
	waitStatus(t, c, helper.ID, model.StatusOffline)
	say(lead, call("fleet_send", map[string]any{"to": "buildbox/helper", "text": "while you were away"}))
	i = wl.waitFor(t, "pending send", i, agentSaidAll("tool fleet_send:", `"state": "pending"`))
	ndB = startNode(t, dirB, stateB)
	defer func() { ndB.stop(t) }()
	waitOnline(t, c, "buildbox")
	wh.waitFor(t, "delivered after reconnect", j, agentSaidAll("from=devbox/lead kind=notify", "while you were away"))
	waitStatus(t, c, helper.ID, model.StatusIdle)
	time.Sleep(2500 * time.Millisecond) // a router retry pass
	if n := countAgent(wh, agentSaidAll("while you were away")); n != 1 {
		t.Fatalf("offline message delivered %d times", n)
	}

	// The traffic log has it all.
	msgs, err := c.Messages(ctx, apiclient.MessageQuery{Session: lead.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, m := range msgs {
		kinds[m.Kind]++
	}
	if kinds[model.MsgAsk] < 3 || kinds[model.MsgReply] < 3 || kinds[model.MsgDone] < 1 || kinds[model.MsgNotify] < 2 {
		t.Fatalf("traffic: %v", kinds)
	}
}

// TestFleetHopLimit checks that two agents that keep answering each other
// are stopped by the hop limit.
func TestFleetHopLimit(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	work := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer co.stop()
	c := apiClient(t, coordDir)
	_, _, nd := enrollHost(t, c, "devbox")
	defer func() { nd.stop(t) }()
	waitOnline(t, c, "devbox")

	// "ping" forwards whatever it gets to "pong" and vice versa (each turn
	// is a CALL fleet_send with the next hop's text), so the chain only
	// ends when the router refuses.
	a, _ := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "ping", Cwd: work})
	b, _ := c.Spawn(ctx, model.SpawnSpec{Host: "devbox", Name: "pong", Cwd: work})
	wa := watch(c, a.ID)
	defer wa.cancel()
	waitStatus(t, c, a.ID, model.StatusIdle)
	waitStatus(t, c, b.ID, model.StatusIdle)
	// Nest the CALLs: each delivered message's last line is the next CALL.
	text := "end"
	for k := 0; k < 12; k++ {
		to := "pong" // the outermost CALL (k=11) runs in ping
		if (11-k)%2 == 1 {
			to = "ping"
		}
		text = call("fleet_send", map[string]any{"to": to, "text": text})
	}
	if _, err := c.Send(ctx, a.ID, text, "queue", ""); err != nil {
		t.Fatal(err)
	}
	wa.waitFor(t, "hop limit", 0, agentSaidAll("tool fleet_send: Error:", "hop limit"))
}
