// Package e2e runs a coordinator and node in-process over real mTLS sockets,
// with the scripted fake app-server standing in for Codex, and drives them
// through the same API client the CLI uses.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/codex/codextest"
	"github.com/jm2/switchyard/internal/coord"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/node"
	"github.com/jm2/switchyard/internal/store"
)

func TestMain(m *testing.M) {
	if codextest.MaybeRun() {
		return
	}
	os.Exit(m.Run())
}

type coordinator struct {
	dir     string
	apiAddr string
	nodeAdd string
	cancel  context.CancelFunc
	srv     *coord.Server
	st      *store.FileStore
	done    chan struct{}
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may still be in TIME_WAIT after a restart
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			return ln
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

func startCoordinator(t *testing.T, dir, apiAddr, nodeAddr string) *coordinator {
	t.Helper()
	apiLn := listen(t, apiAddr)
	nodeLn := listen(t, nodeAddr)
	if _, err := os.Stat(filepath.Join(dir, "coordinator.json")); os.IsNotExist(err) {
		_, apiPort, _ := net.SplitHostPort(apiLn.Addr().String())
		_, nodePort, _ := net.SplitHostPort(nodeLn.Addr().String())
		var ap, np int
		fmt.Sscan(apiPort, &ap)
		fmt.Sscan(nodePort, &np)
		if _, err := coord.Init(dir, []string{"127.0.0.1"}, ap, np); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := coord.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenFileStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := coord.NewServer(dir, cfg, st, func(f string, a ...any) { t.Logf("coord: "+f, a...) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &coordinator{dir: dir, apiAddr: apiLn.Addr().String(), nodeAdd: nodeLn.Addr().String(), cancel: cancel, srv: srv, st: st, done: make(chan struct{})}
	go func() { _ = srv.ServeNodes(nodeLn) }()
	go func() {
		_ = srv.ServeAPI(ctx, apiLn)
		close(c.done)
	}()
	return c
}

func (c *coordinator) stop() {
	c.cancel()
	<-c.done
	c.srv.Shutdown()
	_ = c.st.Close()
}

type runningNode struct {
	n      *node.Node
	cancel context.CancelFunc
	done   chan struct{}
}

func startNode(t *testing.T, dir, fakeState string) *runningNode {
	t.Helper()
	cfg, err := node.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Codex.Bin = os.Args[0]
	cfg.Codex.Args = []string{"app-server"}
	hopts, err := node.HarnessOptions(dir, cfg, func(f string, a ...any) { t.Logf("node: "+f, a...) })
	if err != nil {
		t.Fatal(err)
	}
	hopts.CodexEnv = codextest.Env(fakeState)
	n, err := node.New(dir, cfg, hopts, hopts.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningNode{n: n, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = n.Run(ctx)
		close(r.done)
	}()
	return r
}

func (r *runningNode) stop(t *testing.T) {
	r.cancel()
	<-r.done
	if err := r.n.Close(); err != nil {
		t.Fatal(err)
	}
}

func apiClient(t *testing.T, coordDir string) *apiclient.Client {
	t.Helper()
	cfg, err := apiclient.LoadConfig(filepath.Join(coordDir, "cli.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := apiclient.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitOnline(t *testing.T, c *apiclient.Client, host string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		hosts, err := c.Hosts(context.Background())
		if err == nil {
			for _, h := range hosts {
				if h.ID == host && h.Connected {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host %s never came online", host)
}

// watcher follows a session's SSE stream in the background.
type watcher struct {
	mu     sync.Mutex
	evs    []model.Event
	cancel context.CancelFunc
}

func watch(c *apiclient.Client, id string) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{cancel: cancel}
	go func() {
		_ = c.Stream(ctx, id, 0, func(ev model.Event) bool {
			w.mu.Lock()
			w.evs = append(w.evs, ev)
			w.mu.Unlock()
			return true
		})
	}()
	return w
}

func (w *watcher) waitFor(t *testing.T, what string, from int, pred func(model.Event) bool) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		evs := append([]model.Event(nil), w.evs...)
		w.mu.Unlock()
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				return i
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	for _, e := range w.evs {
		b.WriteString(fmt.Sprintf("\n  %d %s %s", e.Seq, e.Kind, e.Payload))
	}
	t.Fatalf("timed out waiting for %s; stream:%s", what, b.String())
	return -1
}

func (w *watcher) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.evs)
}

func agentSaid(sub string) func(model.Event) bool {
	return func(e model.Event) bool {
		if e.Kind != model.KindItemCompleted {
			return false
		}
		var it model.Item
		_ = json.Unmarshal(e.Payload, &it)
		return it.ItemType == "agentMessage" && strings.Contains(it.Text, sub)
	}
}

func turnEnded(status string) func(model.Event) bool {
	return func(e model.Event) bool {
		if e.Kind != model.KindTurnCompleted {
			return false
		}
		var p model.TurnCompleted
		_ = json.Unmarshal(e.Payload, &p)
		return p.Status == status
	}
}

func kind(k string) func(model.Event) bool { return func(e model.Event) bool { return e.Kind == k } }

func waitStatus(t *testing.T, c *apiclient.Client, id, status string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		s, err := c.Session(context.Background(), id)
		if err == nil {
			last = s.Status
			if s.Status == status {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s: status %q, want %q", id, last, status)
}

func TestFleetEndToEnd(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	nodeDir := t.TempDir()
	fakeState := t.TempDir()
	work := t.TempDir()

	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	c := apiClient(t, coordDir)

	// Enrollment: a one-time token, used once.
	tok, err := c.EnrollToken(ctx, "antec", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Enroll(ctx, nodeDir, tok, false); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Enroll(ctx, t.TempDir(), tok, false); err == nil {
		t.Fatal("enrollment token was accepted twice")
	}
	nd := startNode(t, nodeDir, fakeState)
	waitOnline(t, c, "antec")

	hosts, _ := c.Hosts(ctx)
	if len(hosts) != 1 || len(hosts[0].Adapters) == 0 || hosts[0].Adapters[0] != "codex" {
		t.Fatalf("hosts: %+v", hosts)
	}

	// Spawn with an initial prompt.
	sess, err := c.Spawn(ctx, model.SpawnSpec{Host: "antec", Name: "proj", Cwd: work, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Path != "antec/proj" {
		t.Fatalf("path %s", sess.Path)
	}
	w := watch(c, sess.ID)
	defer w.cancel()
	i := w.waitFor(t, "registration", 0, kind(model.KindSessionRegistered))
	i = w.waitFor(t, "first reply", i, agentSaid("echo: hello"))
	i = w.waitFor(t, "first turn done", i, turnEnded("completed"))
	waitStatus(t, c, sess.ID, model.StatusIdle)

	// Duplicate path is refused while the session is live.
	if _, err := c.Spawn(ctx, model.SpawnSpec{Host: "antec", Name: "proj", Cwd: work}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	// Resolve by path and by unique name.
	if s, err := c.Resolve(ctx, "antec/proj"); err != nil || s.ID != sess.ID {
		t.Fatalf("resolve path: %v", err)
	}
	if s, err := c.Resolve(ctx, "proj"); err != nil || s.ID != sess.ID {
		t.Fatalf("resolve name: %v", err)
	}

	// Two queued messages are delivered one turn at a time.
	if _, err := c.Send(ctx, sess.ID, "second", "queue", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, sess.ID, "third", "queue", ""); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "second reply", i, agentSaid("echo: second"))
	i = w.waitFor(t, "third reply", i, agentSaid("echo: third"))
	i = w.waitFor(t, "third turn done", i, turnEnded("completed"))
	waitStatus(t, c, sess.ID, model.StatusIdle)

	// Approval round trip through the coordinator.
	if _, err := c.Send(ctx, sess.ID, "APPROVE build", "queue", ""); err != nil {
		t.Fatal(err)
	}
	j := w.waitFor(t, "approval request", i, kind(model.KindApprovalRequested))
	var req model.ApprovalRequested
	w.mu.Lock()
	_ = json.Unmarshal(w.evs[j].Payload, &req)
	w.mu.Unlock()
	waitStatus(t, c, sess.ID, model.StatusWaitingApproval)
	if err := c.Approve(ctx, sess.ID, req.ApprovalID, "decline"); err == nil {
		t.Fatal("decline was not offered and must be refused")
	}
	if err := c.Approve(ctx, sess.ID, req.ApprovalID, "accept"); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "approved reply", j, agentSaid("approval decision: accept"))
	i = w.waitFor(t, "approval turn done", i, turnEnded("completed"))
	waitStatus(t, c, sess.ID, model.StatusIdle)

	// Steering into a running turn, then interrupting another.
	if _, err := c.Send(ctx, sess.ID, "SLOW work", "queue", ""); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "slow turn start", i, kind(model.KindTurnStarted))
	if _, err := c.Send(ctx, sess.ID, "focus on tests", "steer", ""); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "steered reply", i, agentSaid("steered: focus on tests"))
	i = w.waitFor(t, "steered turn done", i, turnEnded("completed"))
	waitStatus(t, c, sess.ID, model.StatusIdle)
	if _, err := c.Send(ctx, sess.ID, "SLOW again", "queue", ""); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "second slow turn", i, kind(model.KindTurnStarted))
	waitStatus(t, c, sess.ID, model.StatusActive)
	if err := c.Interrupt(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "interrupted", i, turnEnded("interrupted"))
	waitStatus(t, c, sess.ID, model.StatusIdle)

	// Usage is projected onto the session.
	got, _ := c.Session(ctx, sess.ID)
	if got.Usage == nil || got.Usage.CachedInputTokens != 40 || got.Model != "fake-model" || got.NativeRef == nil {
		t.Fatalf("projection: %+v", got.Session)
	}

	// Node restart: the session is resumed and keeps working.
	nd.stop(t)
	waitStatus(t, c, sess.ID, model.StatusOffline)
	if _, err := c.Send(ctx, sess.ID, "while down", "queue", ""); err == nil {
		t.Fatal("send to an offline host must fail")
	}
	nd = startNode(t, nodeDir, fakeState)
	waitOnline(t, c, "antec")
	i = w.waitFor(t, "resumed registration", i, func(e model.Event) bool {
		var p model.SessionRegistered
		_ = json.Unmarshal(e.Payload, &p)
		return e.Kind == model.KindSessionRegistered && p.Resumed
	})
	waitStatus(t, c, sess.ID, model.StatusIdle)
	if _, err := c.Send(ctx, sess.ID, "after node restart", "queue", ""); err != nil {
		t.Fatal(err)
	}
	i = w.waitFor(t, "reply after node restart", i, agentSaid("echo: after node restart"))
	w.cancel()

	// Coordinator restart: state and events survive; the node reconnects.
	apiAddr, nodeAddr := co.apiAddr, co.nodeAdd
	co.stop()
	co = startCoordinator(t, coordDir, apiAddr, nodeAddr)
	defer co.stop()
	waitOnline(t, c, "antec")
	waitStatus(t, c, sess.ID, model.StatusIdle)
	w2 := watch(c, sess.ID)
	defer w2.cancel()
	w2.waitFor(t, "history replay", 0, agentSaid("echo: after node restart"))
	n0 := w2.len()
	if _, err := c.Send(ctx, sess.ID, "after coordinator restart", "queue", ""); err != nil {
		t.Fatal(err)
	}
	w2.waitFor(t, "reply after coordinator restart", n0, agentSaid("echo: after coordinator restart"))
	w2.waitFor(t, "turn done", n0, turnEnded("completed"))

	// The stored log is gap-free and duplicate-free.
	evs, err := c.Events(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	deltas, completedMsgs := 0, 0
	for k, ev := range evs {
		if ev.Seq != int64(k+1) {
			t.Fatalf("event %d has seq %d", k, ev.Seq)
		}
		if ids[ev.ID] {
			t.Fatalf("duplicate event id %s", ev.ID)
		}
		ids[ev.ID] = true
		if ev.Kind == model.KindItemDelta {
			deltas++
		}
		if agentSaid("")(ev) {
			completedMsgs++
		}
	}
	// The fake streams each reply as three deltas; coalescing must merge
	// them (fewer delta events than three per message).
	if deltas >= 3*completedMsgs {
		t.Fatalf("deltas were not coalesced: %d deltas for %d messages", deltas, completedMsgs)
	}

	// Stop: the session is marked stopped and refuses messages.
	if err := c.Stop(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, sess.ID, model.StatusStopped)
	_, err = c.Send(ctx, sess.ID, "after stop", "queue", "")
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("send after stop: %v", err)
	}
	// The name can be reused once stopped.
	if _, err := c.Spawn(ctx, model.SpawnSpec{Host: "antec", Name: "proj", Cwd: work}); err != nil {
		t.Fatalf("respawn after stop: %v", err)
	}
	nd.stop(t)
}
