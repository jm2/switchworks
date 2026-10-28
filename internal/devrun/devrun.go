// SPDX-License-Identifier: GPL-3.0-or-later

// Package devrun implements `swk dev run`: drive one harness session
// directly from the terminal, without a node or coordinator. It is the
// first smoke test on each host, and with --record it captures the raw protocol
// stream for building fixtures.
package devrun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/adapter/agyad"
	"github.com/jm2/switchworks/internal/adapter/codexad"
	"github.com/jm2/switchworks/internal/harness"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/node"
	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/render"
)

// Main runs `swk dev run [flags] <prompt>` and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	rawStderr := stderr
	stdout, stderr = terminal(stdout, stderr)
	fs := flag.NewFlagSet("dev run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	h := fs.String("harness", "codex", "harness: codex, agy, goose, or the name given with -acp-bin")
	cwd := fs.String("cwd", ".", "working directory for the session")
	modelName := fs.String("model", "", "model override")
	provider := fs.String("provider", "", "model provider override (codex)")
	approval := fs.String("approval", "on-request", "codex approval policy: never, on-request, untrusted")
	sandbox := fs.String("sandbox", "workspace-write", "sandbox: read-only, workspace-write, danger-full-access (agy: danger-full-access adds --dangerously-skip-permissions)")
	codexBin := fs.String("codex-bin", "", "codex binary (default: codex on PATH)")
	codexHome := fs.String("codex-home", "", "CODEX_HOME for app-server (default: codex's own)")
	codexPerms := fs.String("codex-permissions", "", "codex permission profile (config.toml [permissions.<id>]) for -sandbox workspace-write, sent instead of the sandbox mode")
	nodeDir := fs.String("node-dir", paths.NodeDir(), "node directory: codex gets the codex settings of its node.json (bin, args, config, lean, permissions, home), as the node's sessions do; the -codex-* flags override them")
	noNode := fs.Bool("no-node-config", false, "ignore node.json: run codex with its own settings")
	agyBin := fs.String("agy-bin", "", "agy binary (default: agy on PATH)")
	agyPerTurn := fs.Bool("agy-per-turn", false, "agy: one process per turn instead of a long-lived stream-json process")
	gooseBin := fs.String("goose-bin", "", "goose binary (default: goose on PATH)")
	acpBin := fs.String("acp-bin", "", "run -harness NAME as an ACP agent with this binary (e.g. -harness opencode -acp-bin opencode -acp-args acp)")
	acpArgs := fs.String("acp-args", "acp", "space-separated arguments for -acp-bin")
	record := fs.String("record", "", "write the raw protocol stream to this file (JSONL: {dir, line}; mode 0600, on Windows its folder's permissions)")
	autoApprove := fs.String("auto-approve", "", "answer every approval with this decision instead of asking (e.g. accept, cancel)")
	verbose := fs.Bool("v", false, "verbose: show every event")
	logs := fs.Bool("logs", false, "show harness stderr")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: swk dev run [flags] <prompt>\n\nRuns one turn directly against a harness and prints the stream.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fs.Usage()
		return 2
	}
	absCwd, err := filepath.Abs(*cwd)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var rec *recorder
	if *record != "" {
		// The transcript holds prompts, replies and tool output: keep it
		// private like the other state files, also when it replaces one.
		f, err := os.OpenFile(*record, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		if err := f.Chmod(0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		rec = &recorder{w: f}
	}
	harnessStderr := io.Discard
	if *logs {
		harnessStderr = render.Safe(rawStderr)
	}
	opts := harness.Options{AgyBin: *agyBin, Agy: agyad.Options{PerTurn: *agyPerTurn}, Stderr: harnessStderr,
		Logf: func(f string, a ...any) {
			if *verbose {
				fmt.Fprintf(stderr, "swk: "+f+"\n", a...)
			}
		}}
	if *h == codexad.Harness && !*noNode {
		if err := nodeCodexOptions(*nodeDir, &opts, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if *codexBin != "" {
		opts.CodexBin = *codexBin
	}
	if *codexPerms != "" {
		opts.CodexPermissions = *codexPerms
	}
	if *codexHome != "" {
		opts.CodexEnv = map[string]string{"CODEX_HOME": *codexHome}
	}
	opts.ACP = map[string]harness.ACPAgent{}
	if *gooseBin != "" {
		opts.ACP["goose"] = harness.ACPAgent{Bin: *gooseBin}
	}
	if *acpBin != "" {
		opts.ACP[*h] = harness.ACPAgent{Bin: *acpBin, Args: strings.Fields(*acpArgs)}
	}
	if rec != nil {
		opts.Tap = rec.tap
	}
	ad, err := harness.New(*h, opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer ad.Close()

	r := render.New(stdout)
	r.Verbose = *verbose
	events := make(chan adapter.Event, 4096)
	sink := func(e adapter.Event) { events <- e }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := adapter.StartSpec{Name: "dev/run", Cwd: absCwd, Model: *modelName, Provider: *provider,
		ApprovalPolicy: *approval, Sandbox: *sandbox}
	sess, err := ad.Start(ctx, spec, sink)
	if err != nil {
		fmt.Fprintf(stderr, "start %s session: %v\n", *h, err)
		return 1
	}
	if err := sess.Deliver(ctx, adapter.Input{Text: prompt}, adapter.DeliverQueue); err != nil {
		fmt.Fprintf(stderr, "deliver prompt: %v\n", err)
		return 1
	}

	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)
	interrupts := 0
	approvals := newApprover(sess, *autoApprove, stderr)
	var last model.Usage
	for {
		select {
		case <-sigc:
			interrupts++
			if interrupts > 1 {
				fmt.Fprintln(stderr, "\nswk: second interrupt, exiting")
				return 130
			}
			fmt.Fprintln(stderr, "\nswk: interrupting turn (Ctrl-C again to quit)")
			if err := sess.Interrupt(ctx); err != nil && !errors.Is(err, adapter.ErrNotActive) {
				fmt.Fprintf(stderr, "swk: interrupt: %v\n", err)
			}
		case e := <-events:
			payload, _ := json.Marshal(e.Payload)
			r.Event(e.Kind, payload)
			switch e.Kind {
			case model.KindUsage:
				last = e.Payload.(model.UsageEvent).Total
			case model.KindApprovalRequested:
				approvals.handle(ctx, e.Payload.(model.ApprovalRequested))
			case model.KindTurnCompleted:
				tc := e.Payload.(model.TurnCompleted)
				// Drain trailing events briefly so usage is not lost.
				drain(events, r, &last, 200*time.Millisecond)
				r.Usage(last)
				switch tc.Status {
				case "completed":
					return 0
				case "interrupted":
					return 130
				default:
					return 1
				}
			}
		}
	}
}

// nodeCodexOptions gives opts the codex settings of dir's node.json, as
// the node builds them for its sessions, and says on stderr which it
// used: without them dev run tests a different sandbox than sessions get.
func nodeCodexOptions(dir string, opts *harness.Options, stderr io.Writer) error {
	file := filepath.Join(dir, "node.json")
	cfg, err := node.LoadConfig(dir)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "swk: no %s: codex runs with its own settings, not a node's\n", file)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w (pass -no-node-config to run without it)", file, err)
	}
	if err := node.CodexOptions(cfg, opts, opts.Logf); err != nil {
		return fmt.Errorf("%s: %w (pass -no-node-config to run without it)", file, err)
	}
	fmt.Fprintf(stderr, "swk: codex settings from %s, as the node's sessions get them (-no-node-config to ignore)\n", file)
	return nil
}

// terminal filters what dev run and vision-probe print, as cli.Main does
// for the fleet commands: agent and harness text reaches the terminal here,
// and unfiltered it could drive the terminal or redraw the approval prompt
// with a different command. The harness's own stderr (--logs) gets a
// separate render.Safe over the raw writer, so a sequence split in one
// stream cannot join bytes from the other.
func terminal(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	return render.Safe(stdout), render.Safe(stderr)
}

func drain(events chan adapter.Event, r *render.Renderer, last *model.Usage, d time.Duration) {
	t := time.After(d)
	for {
		select {
		case e := <-events:
			payload, _ := json.Marshal(e.Payload)
			r.Event(e.Kind, payload)
			if e.Kind == model.KindUsage {
				*last = e.Payload.(model.UsageEvent).Total
			}
		case <-t:
			return
		}
	}
}

type recorder struct {
	mu sync.Mutex
	w  io.Writer
}

func (r *recorder) tap(dir string, line []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := struct {
		TS   string          `json:"ts"`
		Dir  string          `json:"dir"`
		Line json.RawMessage `json:"line"`
	}{time.Now().UTC().Format(time.RFC3339Nano), dir, nil}
	if json.Valid(line) {
		rec.Line = line
	} else {
		b, _ := json.Marshal(string(line))
		rec.Line = b
	}
	b, _ := json.Marshal(rec)
	_, _ = r.w.Write(append(b, '\n'))
}

type approver struct {
	h      adapter.Handle
	auto   string
	stderr io.Writer
	in     *bufio.Reader
}

func newApprover(h adapter.Handle, auto string, stderr io.Writer) *approver {
	return &approver{h: h, auto: auto, stderr: stderr, in: bufio.NewReader(os.Stdin)}
}

func (a *approver) handle(ctx context.Context, req model.ApprovalRequested) {
	decide := func(d string) {
		if err := a.h.Resolve(ctx, req.ApprovalID, d); err != nil {
			fmt.Fprintf(a.stderr, "swk: resolve: %v\n", err)
		}
	}
	if a.auto != "" {
		decide(a.auto)
		return
	}
	options := req.Decisions
	if len(options) == 0 {
		options = []string{"accept", "acceptForSession", "decline", "cancel"}
	}
	var simple []string
	for _, o := range options {
		switch o {
		case "accept", "acceptForSession", "decline", "declineAlways", "cancel":
			simple = append(simple, o)
		}
	}
	go func() {
		for {
			fmt.Fprintf(a.stderr, "decision [%s]: ", strings.Join(simple, "/"))
			line, err := a.in.ReadString('\n')
			if err != nil {
				fmt.Fprintln(a.stderr, "\nswk: stdin closed; cancelling")
				decide("cancel")
				return
			}
			if o := pickDecision(simple, strings.TrimSpace(line)); o != "" {
				decide(o)
				return
			}
		}
	}()
}

// pickDecision matches typed input to an offered decision: an exact name,
// or a single letter for the shortest decision starting with it ("a" is
// accept, not acceptForSession).
func pickDecision(options []string, d string) string {
	best := ""
	for _, o := range options {
		if d == o {
			return o
		}
		if len(d) == 1 && strings.HasPrefix(o, d) && (best == "" || len(o) < len(best)) {
			best = o
		}
	}
	return best
}
