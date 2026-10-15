// SPDX-License-Identifier: GPL-3.0-or-later

// Package render prints a session's event stream to a terminal. It is shared
// by `swyd dev run` (events straight from an adapter) and `swyd attach`
// (events streamed from the coordinator).
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/jm2/switchyard/internal/model"
)

// Renderer writes human-readable output for events.
type Renderer struct {
	W       io.Writer
	Verbose bool // show raw item types and status changes

	mu        sync.Mutex
	streaming map[string]bool // item ids that already printed deltas
	midLine   bool
}

// New returns a renderer writing to w.
func New(w io.Writer) *Renderer { return &Renderer{W: w, streaming: map[string]bool{}} }

func (r *Renderer) line(format string, args ...any) {
	if r.midLine {
		fmt.Fprintln(r.W)
		r.midLine = false
	}
	fmt.Fprintf(r.W, format+"\n", args...)
}

// Event renders one event. payload is the event's JSON payload.
func (r *Renderer) Event(kind string, payload json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case model.KindSessionRegistered:
		var p model.SessionRegistered
		_ = json.Unmarshal(payload, &p)
		verb := "started"
		if p.Resumed {
			verb = "resumed"
		}
		r.line("── session %s: %s %s (model %s via %s, cwd %s)", verb, p.NativeRef.Harness, p.NativeRef.ID, orDash(p.Model), orDash(p.Provider), orDash(p.Cwd))
	case model.KindSessionStatus:
		var p model.SessionStatus
		_ = json.Unmarshal(payload, &p)
		if r.Verbose || p.Status == model.StatusError || p.Status == model.StatusGone || p.Status == model.StatusStopped || p.Detail != "" {
			msg := "── status: " + p.Status
			if p.Detail != "" {
				msg += " (" + p.Detail + ")"
			}
			r.line("%s", msg)
		}
	case model.KindTurnStarted:
		if r.Verbose {
			var p model.TurnStarted
			_ = json.Unmarshal(payload, &p)
			r.line("── turn %s started", p.TurnID)
		}
	case model.KindItemDelta:
		var p model.ItemDelta
		_ = json.Unmarshal(payload, &p)
		r.streaming[p.ItemID] = true
		fmt.Fprint(r.W, p.Text)
		r.midLine = !strings.HasSuffix(p.Text, "\n")
	case model.KindItemStarted:
		var p model.Item
		_ = json.Unmarshal(payload, &p)
		switch p.ItemType {
		case "commandExecution":
			var c struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(p.Raw, &c)
			r.line("$ %s", c.Command)
		case "userMessage":
			var u struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.Unmarshal(p.Raw, &u)
			for _, c := range u.Content {
				if c.Type != "text" {
					continue
				}
				// A routed message: "[switchyard msg=... from=... kind=...]\n..."
				if hdr, body, ok := strings.Cut(c.Text, "\n"); ok && strings.HasPrefix(hdr, "[switchyard ") && strings.HasSuffix(hdr, "]") {
					r.line("✉ %s", strings.TrimSuffix(strings.TrimPrefix(hdr, "[switchyard "), "]"))
					for _, l := range strings.Split(body, "\n") {
						r.line("  %s", l)
					}
					continue
				}
				r.line("> %s", c.Text)
			}
		case "mcpToolCall":
			var m struct {
				Server    string          `json:"server"`
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(p.Raw, &m)
			r.line("⇄ %s.%s %s", m.Server, m.Tool, clip(string(m.Arguments), 160))
		case "toolCall":
			var m struct {
				Title string `json:"title"`
				Tool  string `json:"tool"`
			}
			_ = json.Unmarshal(p.Raw, &m)
			r.line("⚙ %s", orStr(m.Title, m.Tool))
		default:
			if r.Verbose {
				r.line("── %s started", p.ItemType)
			}
		}
	case model.KindItemCompleted:
		var p model.Item
		_ = json.Unmarshal(payload, &p)
		switch p.ItemType {
		case "agentMessage":
			if r.streaming[p.ItemID] {
				delete(r.streaming, p.ItemID)
				if r.midLine {
					fmt.Fprintln(r.W)
					r.midLine = false
				}
			} else if p.Text != "" {
				r.line("%s", p.Text)
			}
		case "commandExecution":
			var c struct {
				Status   string `json:"status"`
				ExitCode *int   `json:"exitCode"`
			}
			_ = json.Unmarshal(p.Raw, &c)
			code := "?"
			if c.ExitCode != nil {
				code = fmt.Sprint(*c.ExitCode)
			}
			r.line("  └ %s (exit %s)", c.Status, code)
		case "fileChange":
			var c struct {
				Status  string `json:"status"`
				Changes []struct {
					Path string `json:"path"`
				} `json:"changes"`
			}
			_ = json.Unmarshal(p.Raw, &c)
			paths := make([]string, 0, len(c.Changes))
			for _, ch := range c.Changes {
				paths = append(paths, ch.Path)
			}
			r.line("  ✎ %s: %s", c.Status, strings.Join(paths, ", "))
		case "mcpToolCall":
			var m struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
				Result *struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			_ = json.Unmarshal(p.Raw, &m)
			out := ""
			if m.Error != nil {
				out = m.Error.Message
			} else if m.Result != nil {
				for _, c := range m.Result.Content {
					if c.Type == "text" {
						out = c.Text
						break
					}
				}
			}
			r.line("  └ %s: %s", m.Status, clip(out, 200))
		case "toolCall":
			var m struct {
				Status string `json:"status"`
				Output string `json:"output"`
			}
			_ = json.Unmarshal(p.Raw, &m)
			if m.Output != "" {
				r.line("  └ %s: %s", m.Status, clip(m.Output, 200))
			} else {
				r.line("  └ %s", m.Status)
			}
		case "reasoning":
			if r.Verbose && p.Text != "" {
				r.line("── thinking: %s", clip(p.Text, 400))
			}
		case "plan":
			var m struct {
				Entries []struct {
					Content string `json:"content"`
					Status  string `json:"status"`
				} `json:"entries"`
			}
			_ = json.Unmarshal(p.Raw, &m)
			r.line("── plan")
			for _, e := range m.Entries {
				mark := "·"
				switch e.Status {
				case "completed":
					mark = "✓"
				case "in_progress":
					mark = "▸"
				}
				r.line("  %s %s", mark, e.Content)
			}
		case "userMessage":
		default:
			if r.Verbose {
				r.line("── %s completed", p.ItemType)
			}
		}
	case model.KindTurnDiff:
		if r.Verbose {
			var p model.TurnDiff
			_ = json.Unmarshal(payload, &p)
			r.line("── diff updated (%d bytes)", len(p.Diff))
		}
	case model.KindApprovalRequested:
		var p model.ApprovalRequested
		_ = json.Unmarshal(payload, &p)
		what := p.Command
		if p.Kind == "file_change" && what == "" {
			what = "file changes"
		}
		r.line("!! approval needed [%s] %s: %s", p.ApprovalID, p.Kind, what)
		if p.Reason != "" {
			r.line("   reason: %s", p.Reason)
		}
		if len(p.Decisions) > 0 {
			r.line("   offered: %s", strings.Join(p.Decisions, ", "))
		}
	case model.KindApprovalResolved:
		var p model.ApprovalResolved
		_ = json.Unmarshal(payload, &p)
		r.line("── approval %s: %s", p.ApprovalID, p.Decision)
	case model.KindUsage:
		var p model.UsageEvent
		_ = json.Unmarshal(payload, &p)
		if r.Verbose {
			r.line("── usage (this call): in %d (cached %d), out %d", p.Last.InputTokens, p.Last.CachedInputTokens, p.Last.OutputTokens)
		}
	case model.KindTurnCompleted:
		var p model.TurnCompleted
		_ = json.Unmarshal(payload, &p)
		msg := "── turn " + p.Status
		if p.DurationMs > 0 {
			msg += fmt.Sprintf(" in %.1fs", float64(p.DurationMs)/1000)
		}
		if p.Error != "" {
			msg += ": " + p.Error
		}
		r.line("%s", msg)
	case model.KindMessageQueued, model.KindMessageDelivered, model.KindMessageFailed:
		var p model.MessageEvent
		_ = json.Unmarshal(payload, &p)
		switch kind {
		case model.KindMessageQueued:
			if r.Verbose {
				r.line("── message %s queued from %s", p.MessageID, p.From)
			}
		case model.KindMessageFailed:
			r.line("!! message %s from %s failed: %s", p.MessageID, p.From, p.Error)
		}
	case model.KindSessionSettled:
		if r.Verbose {
			r.line("── settled: all queued work done")
		}
	case model.KindError:
		var p model.ErrorEvent
		_ = json.Unmarshal(payload, &p)
		retry := ""
		if p.WillRetry {
			retry = " (retrying)"
		}
		r.line("!! error%s: %s", retry, p.Message)
	case model.KindWarning:
		var p model.ErrorEvent
		_ = json.Unmarshal(payload, &p)
		r.line("!! warning: %s", p.Message)
	default:
		if r.Verbose {
			r.line("── %s %s", kind, string(payload))
		}
	}
}

// Usage prints a usage summary line.
func (r *Renderer) Usage(u model.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pct := 0.0
	if u.InputTokens > 0 {
		pct = 100 * float64(u.CachedInputTokens) / float64(u.InputTokens)
	}
	r.line("── tokens: in %d (cached %d, %.0f%%), out %d, reasoning %d, total %d",
		u.InputTokens, u.CachedInputTokens, pct, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
}

// clip flattens s to one line of at most n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
