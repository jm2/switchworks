// SPDX-License-Identifier: GPL-3.0-or-later

// Package cli implements the fleet commands of `swyd` (ls, spawn, attach,
// send, ...), all through the coordinator's HTTPS API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/paths"
	"github.com/jm2/switchyard/internal/render"
)

// Main dispatches a fleet command.
func Main(args []string, stdout, stderr io.Writer) int {
	cmd, rest := args[0], args[1:]
	fn, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q (see `swyd help`)\n", cmd)
		return 2
	}
	// Fleet commands print agents' and models' text: keep it from
	// controlling the terminal.
	return fn(rest, render.Safe(stdout), render.Safe(stderr))
}

type command func(args []string, stdout, stderr io.Writer) int

var commands = map[string]command{
	"ls":        cmdLs,
	"hosts":     cmdHosts,
	"spawn":     cmdSpawn,
	"attach":    cmdAttach,
	"send":      cmdSend,
	"interrupt": cmdInterrupt,
	"approve":   cmdApprove,
	"stop":      cmdStop,
	"events":    cmdEvents,
}

func client(stderr io.Writer) (*apiclient.Client, bool) {
	cfg, err := apiclient.LoadConfig(paths.CLIConfig())
	if err != nil {
		fmt.Fprintf(stderr, "swyd: no CLI config (%v)\n  make one where the admin token is: swyd token create <name> --scope operate --cli-config > %s\n", err, paths.CLIConfig())
		return nil, false
	}
	c, err := apiclient.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, false
	}
	return c, true
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "swyd:", err)
	return 1
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func tokens(u *model.Usage) string {
	if u == nil {
		return "-"
	}
	return fmt.Sprintf("%s/%s/%s", human(u.InputTokens), human(u.CachedInputTokens), human(u.OutputTokens))
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func cmdLs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "include stopped sessions")
	asJSON := fs.Bool("json", false, "print JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	sessions, err := c.Sessions(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(sessions)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tSTATUS\tHARNESS\tMODEL\tTOKENS in/cached/out\tUPDATED\tID")
	for _, s := range sessions {
		if !*all && (s.Status == model.StatusStopped || s.Status == model.StatusGone) {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Path, s.Status, s.Harness, orDash(s.Model), tokens(s.Usage), ago(s.UpdatedAt), s.ID)
	}
	tw.Flush()
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdHosts(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "rm" || args[0] == "remove") {
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: swyd hosts rm <host>")
			return 2
		}
		c, ok := client(stderr)
		if !ok {
			return 1
		}
		ctx, cancel := ctxTimeout()
		defer cancel()
		if err := c.RemoveHost(ctx, args[1]); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "removed %s: its certificate no longer connects and its sessions are marked gone; enroll it again to bring it back\n", args[1])
		return 0
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	hosts, err := c.Hosts(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSTATE\tPLATFORM\tVERSION\tHARNESSES\tLAST SEEN")
	for _, h := range hosts {
		state := "offline"
		if h.Connected {
			state = "online"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s/%s\t%s\t%s\t%s\n", h.ID, state, orDash(h.OS), orDash(h.Arch), orDash(h.Version), strings.Join(h.Adapters, ","), ago(h.LastSeen))
	}
	tw.Flush()
	return 0
}

var nonName = regexp.MustCompile(`[^a-z0-9._-]+`)

func defaultName(cwd string) string {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(cwd)))
	if i := strings.LastIndex(cwd, "\\"); i >= 0 { // Windows path from another host
		base = strings.ToLower(cwd[i+1:])
	}
	base = strings.Trim(nonName.ReplaceAllString(base, "-"), "-.")
	if base == "" {
		base = "session"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	return base
}

// interleave lets flags follow positional arguments ("spawn devbox --cwd x").
func interleave(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos, nil
}

func cmdSpawn(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "session name (default: the cwd's base name)")
	cwd := fs.String("cwd", "", "working directory on the target host (required)")
	h := fs.String("harness", "codex", "harness: codex, agy, goose, or an ACP agent named in the node's node.json (see `swyd hosts`)")
	modelName := fs.String("model", "", "model")
	provider := fs.String("provider", "", "model provider (codex)")
	approval := fs.String("approval", "", "codex approval policy: never, on-request, untrusted (default: node's)")
	sandbox := fs.String("sandbox", "", "read-only, workspace-write, danger-full-access (default: node's)")
	doAttach := fs.Bool("attach", false, "attach after spawning")
	images := addImageFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: swyd spawn <host> --cwd DIR [--image FILE]... [flags] [prompt]")
		fs.PrintDefaults()
	}
	pos, err := interleave(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) < 1 || *cwd == "" {
		fs.Usage()
		return 2
	}
	host := pos[0]
	prompt := strings.Join(pos[1:], " ")
	if *name == "" {
		*name = defaultName(*cwd)
	}
	atts, err := readImages(*images)
	if err != nil {
		return fail(stderr, err)
	}
	if len(atts) > 0 && strings.TrimSpace(prompt) == "" {
		return fail(stderr, errors.New("--image needs a prompt to go with it"))
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	sess, err := c.Spawn(ctx, model.SpawnSpec{Host: host, Name: *name, Harness: *h, Cwd: *cwd, Prompt: prompt, Attachments: atts,
		Model: *modelName, Provider: *provider, ApprovalPolicy: *approval, Sandbox: *sandbox})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "spawned %s (%s)\n", sess.Path, sess.ID)
	if *doAttach {
		return attach(c, sess.ID, 0, false, false, stdout, stderr)
	}
	return 0
}

func cmdAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tail := fs.Int("tail", 200, "replay this many past events (0 = from the start)")
	untilIdle := fs.Bool("until-idle", false, "exit when the session's current turn completes")
	verbose := fs.Bool("v", false, "show every event")
	pos, err := interleave(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: swyd attach [--tail N] [--until-idle] [-v] <session>")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	sess, err := c.Resolve(ctx, pos[0])
	cancel()
	if err != nil {
		return fail(stderr, err)
	}
	after := int64(0)
	if *tail > 0 && sess.LastSeq > int64(*tail) {
		after = sess.LastSeq - int64(*tail)
	}
	fmt.Fprintf(stderr, "── attached to %s (%s, %s); Ctrl-C to detach\n", sess.Path, sess.Status, sess.ID)
	return attach(c, sess.ID, after, *untilIdle, *verbose, stdout, stderr)
}

func attach(c *apiclient.Client, id string, after int64, untilIdle, verbose bool, stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	r := render.New(stdout)
	r.Verbose = verbose
	sawTurn := false
	err := c.Stream(ctx, id, after, func(ev model.Event) bool {
		r.Event(ev.Kind, ev.Payload)
		switch ev.Kind {
		case model.KindApprovalRequested:
			var p model.ApprovalRequested
			_ = json.Unmarshal(ev.Payload, &p)
			fmt.Fprintf(stderr, "   answer with: swyd approve %s %s <decision>\n", id, p.ApprovalID)
		case model.KindTurnStarted:
			sawTurn = true
		case model.KindTurnCompleted:
			if untilIdle && sawTurn {
				return false
			}
		case model.KindUsage:
			if verbose {
				var p model.UsageEvent
				_ = json.Unmarshal(ev.Payload, &p)
				r.Usage(p.Total)
			}
		}
		return true
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fail(stderr, err)
	}
	return 0
}

func resolve(c *apiclient.Client, ref string, stderr io.Writer) (apiclient.SessionView, bool) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	s, err := c.Resolve(ctx, ref)
	if err != nil {
		fail(stderr, err)
		return s, false
	}
	return s, true
}

func cmdSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	steer := fs.Bool("steer", false, "inject into the running turn instead of queueing a new one")
	intr := fs.Bool("interrupt", false, "cancel the running turn and deliver now")
	from := fs.String("from", "", "sender (default user:cli)")
	images := addImageFlag(fs)
	pos, err := interleave(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: swyd send [--steer|--interrupt] [--image FILE]... <session> <text>")
		return 2
	}
	atts, err := readImages(*images)
	if err != nil {
		return fail(stderr, err)
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	s, ok := resolve(c, pos[0], stderr)
	if !ok {
		return 1
	}
	mode := model.ModeQueue
	switch {
	case *steer && *intr:
		fmt.Fprintln(stderr, "swyd send: --steer and --interrupt are exclusive")
		return 2
	case *steer:
		mode = model.ModeSteer
	case *intr:
		mode = model.ModeInterrupt
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	res, err := c.Send(ctx, s.ID, strings.Join(pos[1:], " "), mode, *from, atts...)
	if err != nil {
		return fail(stderr, err)
	}
	how := map[string]string{model.ModeQueue: "queued", model.ModeSteer: "steered", model.ModeInterrupt: "interrupted with"}[res.Mode]
	if how == "" {
		how = "queued"
	}
	if res.State == model.MsgPending {
		how = "holding (host offline)"
	}
	fmt.Fprintf(stdout, "%s message %s to %s\n", how, res.MessageID, s.Path)
	return 0
}

func simple(name string, fn func(context.Context, *apiclient.Client, string) error) command {
	return func(args []string, stdout, stderr io.Writer) int {
		if len(args) != 1 {
			fmt.Fprintf(stderr, "usage: swyd %s <session>\n", name)
			return 2
		}
		c, ok := client(stderr)
		if !ok {
			return 1
		}
		s, ok := resolve(c, args[0], stderr)
		if !ok {
			return 1
		}
		ctx, cancel := ctxTimeout()
		defer cancel()
		if err := fn(ctx, c, s.ID); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s: %s\n", name, s.Path)
		return 0
	}
}

var cmdInterrupt = simple("interrupt", func(ctx context.Context, c *apiclient.Client, id string) error { return c.Interrupt(ctx, id) })
var cmdStop = simple("stop", func(ctx context.Context, c *apiclient.Client, id string) error { return c.Stop(ctx, id) })

func cmdApprove(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "usage: swyd approve <session> <approval-id> <decision>\n  decisions: accept, acceptForSession, decline, cancel (as offered)")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	s, ok := resolve(c, args[0], stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	if err := c.Approve(ctx, s.ID, args[1], args[2]); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "approval %s: %s\n", args[1], args[2])
	return 0
}

func cmdEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	after := fs.Int64("after", 0, "only events with seq above this")
	pos, err := interleave(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: swyd events [--after N] <session>")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	s, ok := resolve(c, pos[0], stderr)
	if !ok {
		return 1
	}
	enc := json.NewEncoder(stdout)
	for {
		ctx, cancel := ctxTimeout()
		evs, err := c.Events(ctx, s.ID, *after, 5000)
		cancel()
		if err != nil {
			return fail(stderr, err)
		}
		for _, ev := range evs {
			_ = enc.Encode(ev)
			*after = ev.Seq
		}
		if len(evs) == 0 { // a page can be short when its events are large
			return 0
		}
	}
}
