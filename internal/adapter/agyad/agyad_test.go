package agyad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/model"
)

// The fake agy below emits only the documented top-level event types (init,
// step_update, result) with the documented result fields (conversation_id,
// status, response, usage, duration_seconds). Field names inside
// step_update are a guess; the adapter treats them tolerantly.
func TestMain(m *testing.M) {
	if os.Getenv("SWYD_FAKE_AGY") == "1" {
		fakeAgy()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAgy() {
	args := os.Args[1:]
	conv := "conv-new"
	skip := false
	for i, a := range args {
		if a == "--conversation" && i+1 < len(args) {
			conv = args[i+1]
		}
		if a == "--dangerously-skip-permissions" {
			skip = true
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	prompt := string(in)
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	emit(map[string]any{"type": "init", "conversation_id": conv})
	switch {
	case strings.Contains(prompt, "SLOW"):
		time.Sleep(30 * time.Second)
	case strings.Contains(prompt, "NORESULT"):
		os.Exit(2)
	case strings.Contains(prompt, "FAIL"):
		emit(map[string]any{"type": "result", "status": "ERROR", "error": "boom", "conversation_id": conv})
		return
	}
	reply := fmt.Sprintf("echo: %s (conv=%s skip=%v)", prompt, conv, skip)
	for _, part := range []string{reply[:5], reply[5:]} {
		emit(map[string]any{"type": "step_update", "text": part})
	}
	emit(map[string]any{"type": "result", "status": "SUCCESS", "response": reply, "conversation_id": conv,
		"duration_seconds": 1.5, "num_turns": 1, "usage": map[string]any{"input_tokens": 10, "output_tokens": 3}})
}

type collector struct {
	mu  sync.Mutex
	evs []adapter.Event
}

func (c *collector) sink(e adapter.Event) {
	c.mu.Lock()
	c.evs = append(c.evs, e)
	c.mu.Unlock()
}

func (c *collector) waitFor(t *testing.T, from int, what string, pred func(adapter.Event) bool) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		evs := append([]adapter.Event(nil), c.evs...)
		c.mu.Unlock()
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				return i
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, e := range c.evs {
		p, _ := json.Marshal(e.Payload)
		b.WriteString("\n  " + e.Kind + " " + string(p))
	}
	t.Fatalf("timed out waiting for %s; events:%s", what, b.String())
	return -1
}

func (c *collector) at(i int) adapter.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evs[i]
}

func turnDone(status string) func(adapter.Event) bool {
	return func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == status
	}
}

func newAdapter(t *testing.T) *Adapter {
	a := New(Config{Bin: os.Args[0], Env: map[string]string{"SWYD_FAKE_AGY": "1"}, Logf: t.Logf})
	t.Cleanup(func() { a.Close() })
	return a
}

func TestTurnsConversationAndUsage(t *testing.T) {
	a := newAdapter(t)
	ctx := context.Background()
	col := &collector{}
	h, err := a.Start(ctx, adapter.StartSpec{Cwd: t.TempDir()}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ref().ID != "" {
		t.Fatal("conversation id must be unknown before the first turn")
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "hi"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "registration with conversation id", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionRegistered)
		return ok && p.NativeRef.ID == "conv-new"
	})
	i = col.waitFor(t, i, "delta", func(e adapter.Event) bool { return e.Kind == model.KindItemDelta })
	j := col.waitFor(t, i, "agent message", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.Item)
		return ok && p.ItemType == "agentMessage" && strings.HasPrefix(p.Text, "echo: hi")
	})
	k := col.waitFor(t, j, "usage", func(e adapter.Event) bool { return e.Kind == model.KindUsage })
	if u := col.at(k).Payload.(model.UsageEvent); u.Total.InputTokens != 10 || u.Total.TotalTokens != 13 {
		t.Fatalf("usage %+v", u.Total)
	}
	k = col.waitFor(t, k, "completed", turnDone("completed"))
	if d := col.at(k).Payload.(model.TurnCompleted).DurationMs; d != 1500 {
		t.Fatalf("duration %d", d)
	}
	col.waitFor(t, k, "idle", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.SessionStatus)
		return ok && p.Status == model.StatusIdle
	})

	// Second turn continues the conversation.
	if err := h.Deliver(ctx, adapter.Input{Text: "again"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, k, "continued conversation", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.Item)
		return ok && p.ItemType == "agentMessage" && strings.Contains(p.Text, "conv=conv-new")
	})
	if err := h.Deliver(ctx, adapter.Input{Text: "x"}, adapter.DeliverSteer); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("steer: %v", err)
	}
	if err := h.Resolve(ctx, "a", "accept"); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("resolve: %v", err)
	}
}

func TestPermissionsFailuresAndInterrupt(t *testing.T) {
	a := newAdapter(t)
	ctx := context.Background()
	col := &collector{}
	h, err := a.Resume(ctx, model.NativeRef{Harness: "agy", ID: "conv-7"}, adapter.StartSpec{Cwd: t.TempDir(), Sandbox: "danger-full-access"}, col.sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "perm"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i := col.waitFor(t, 0, "skip flag and resumed id", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.Item)
		return ok && p.ItemType == "agentMessage" && strings.Contains(p.Text, "conv=conv-7 skip=true")
	})
	i = col.waitFor(t, i, "turn 1 done", turnDone("completed"))
	waitIdle(t, h)

	if err := h.Deliver(ctx, adapter.Input{Text: "FAIL"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i = col.waitFor(t, i+1, "failed turn", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == "failed" && p.Error == "boom"
	})
	waitIdle(t, h)

	if err := h.Deliver(ctx, adapter.Input{Text: "NORESULT"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	i = col.waitFor(t, i+1, "missing result", func(e adapter.Event) bool {
		p, ok := e.Payload.(model.TurnCompleted)
		return ok && p.Status == "failed" && strings.Contains(p.Error, "without a result")
	})
	waitIdle(t, h)

	if err := h.Deliver(ctx, adapter.Input{Text: "SLOW"}, adapter.DeliverQueue); err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, adapter.Input{Text: "busy"}, adapter.DeliverQueue); !errors.Is(err, adapter.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if err := h.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, i+1, "interrupted", turnDone("interrupted"))
}

func waitIdle(t *testing.T, h adapter.Handle) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.Status() == model.StatusIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status %s", h.Status())
}
