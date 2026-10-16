// SPDX-License-Identifier: GPL-3.0-or-later

package devrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/harness"
	"github.com/jm2/switchworks/internal/model"
)

// The vision probe checks whether a harness and model can see images: it
// draws a random code into a PNG, sends it with "Reply with only the code
// in this image", and compares the answer. The code is also stored in the
// PNG's tEXt chunk, which no real model reads but the mock provider and
// the fake harnesses do, so the probe can be tested without a model.

// probeAlphabet avoids look-alikes (0/O/D/Q, 1/I, 2/Z, 5/S, 6/G, 8/B).
const probeAlphabet = "ACEFHJKLMNPRTUVWXY3479"

// ProbePrompt is the probe's question.
const ProbePrompt = "Reply with only the code in this image."

// glyphs is a 5x7 bitmap font for probeAlphabet.
var glyphs = map[rune][7]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'C': {".####", "#....", "#....", "#....", "#....", "#....", ".####"},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'J': {"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'3': {"####.", "....#", "....#", ".###.", "....#", "....#", "####."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "....#", ".###."},
}

// NewProbeCode returns a random code of n characters from probeAlphabet.
func NewProbeCode(n int) string {
	var b strings.Builder
	max := big.NewInt(int64(len(probeAlphabet)))
	for i := 0; i < n; i++ {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(probeAlphabet[k.Int64()])
	}
	return b.String()
}

// ProbePNG draws code (characters of probeAlphabet) in black on white,
// scaled up, and stores it in a tEXt chunk too.
func ProbePNG(code string) ([]byte, error) {
	const scale, margin = 12, 3 // pixels per font cell; cells of white around the text
	cols := len(code)*6 - 1 + 2*margin
	rows := 7 + 2*margin
	img := image.NewGray(image.Rect(0, 0, cols*scale, rows*scale))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	for i, r := range code {
		g, ok := glyphs[r]
		if !ok {
			return nil, fmt.Errorf("no glyph for %q", r)
		}
		for y, row := range g {
			for x, c := range row {
				if c != '#' {
					continue
				}
				x0, y0 := (margin+i*6+x)*scale, (margin+y)*scale
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetGray(x0+dx, y0+dy, color.Gray{Y: 0})
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return attach.AddPNGText(buf.Bytes(), attach.ProbeKey, code), nil
}

// ProbeResult is the outcome of one probe.
type ProbeResult struct {
	Harness  string
	Model    string
	Provider string
	Code     string
	Answer   string
	Pass     bool
	Duration time.Duration
}

// normalizeAnswer keeps the letters and digits of an answer, uppercased.
func normalizeAnswer(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Probe runs one vision probe against a harness: it starts a session,
// sends the probe image, waits for the turn and checks the answer.
// Approvals are declined (the probe needs no tools).
func Probe(ctx context.Context, opts harness.Options, name string, spec adapter.StartSpec, logf func(string, ...any)) (ProbeResult, error) {
	res := ProbeResult{Harness: name, Code: NewProbeCode(6)}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	img, err := ProbePNG(res.Code)
	if err != nil {
		return res, err
	}
	images, err := attach.Normalize([]model.Attachment{{Name: "vision-probe.png", Data: img}})
	if err != nil {
		return res, err
	}
	ad, err := harness.New(name, opts)
	if err != nil {
		return res, err
	}
	defer ad.Close()
	events := make(chan adapter.Event, 4096)
	start := time.Now()
	h, err := ad.Start(ctx, spec, func(e adapter.Event) {
		select {
		case events <- e:
		default:
		}
	})
	if err != nil {
		return res, fmt.Errorf("start %s session: %w", name, err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.Stop(sctx)
	}()
	if err := h.Deliver(ctx, adapter.Input{Text: ProbePrompt, Images: images}, adapter.DeliverQueue); err != nil {
		return res, fmt.Errorf("send the probe image: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("no answer: %w", ctx.Err())
		case e := <-events:
			switch e.Kind {
			case model.KindSessionRegistered:
				p := e.Payload.(model.SessionRegistered)
				res.Model, res.Provider = p.Model, p.Provider
			case model.KindUsage:
				if p := e.Payload.(model.UsageEvent); p.Model != "" {
					res.Model = p.Model
				}
			case model.KindItemCompleted:
				if p := e.Payload.(model.Item); p.ItemType == "agentMessage" && p.Text != "" {
					res.Answer = p.Text
				}
			case model.KindApprovalRequested:
				p := e.Payload.(model.ApprovalRequested)
				logf("declining approval %s (%s)", p.ApprovalID, p.Command)
				_ = h.Resolve(ctx, p.ApprovalID, "decline")
			case model.KindError, model.KindWarning:
				if b, err := json.Marshal(e.Payload); err == nil {
					logf("%s: %s", e.Kind, b)
				}
			case model.KindTurnCompleted:
				tc := e.Payload.(model.TurnCompleted)
				res.Duration = time.Since(start)
				// The agent message may complete just after the turn.
				drainFor(events, 200*time.Millisecond, func(e adapter.Event) {
					if e.Kind == model.KindItemCompleted {
						if p := e.Payload.(model.Item); p.ItemType == "agentMessage" && p.Text != "" {
							res.Answer = p.Text
						}
					}
				})
				if tc.Status != "completed" {
					return res, fmt.Errorf("turn %s: %s", tc.Status, tc.Error)
				}
				ans := normalizeAnswer(res.Answer)
				res.Pass = ans != "" && strings.Contains(ans, res.Code) && len(ans) <= len(res.Code)+40
				return res, nil
			}
		}
	}
}

func drainFor(events chan adapter.Event, d time.Duration, fn func(adapter.Event)) {
	t := time.After(d)
	for {
		select {
		case e := <-events:
			fn(e)
		case <-t:
			return
		}
	}
}

// VisionMain runs `swk dev vision-probe`.
func VisionMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev vision-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	h := fs.String("harness", "codex", "harness: codex, goose, or the name given with -acp-bin (agy cannot take images)")
	modelName := fs.String("model", "", "model override")
	provider := fs.String("provider", "", "model provider override")
	cwd := fs.String("cwd", "", "working directory for the session (default: a new empty temporary directory)")
	codexBin := fs.String("codex-bin", "", "codex binary (default: codex on PATH)")
	codexHome := fs.String("codex-home", "", "CODEX_HOME for app-server (default: codex's own)")
	gooseBin := fs.String("goose-bin", "", "goose binary (default: goose on PATH)")
	acpBin := fs.String("acp-bin", "", "run -harness NAME as an ACP agent with this binary")
	acpArgs := fs.String("acp-args", "acp", "space-separated arguments for -acp-bin")
	timeout := fs.Duration("timeout", 3*time.Minute, "give up after this long")
	save := fs.String("save", "", "also write the probe image to this file")
	verbose := fs.Bool("v", false, "verbose")
	logs := fs.Bool("logs", false, "show harness stderr")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: swk dev vision-probe [flags]\n\nSends an image with a random code to a harness and checks that the model reads it back.\nIt runs the harness directly with your own configuration and keys, like `swk dev run`.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	logf := func(f string, a ...any) {
		if *verbose {
			fmt.Fprintf(stderr, "swk: "+f+"\n", a...)
		}
	}
	harnessStderr := io.Discard
	if *logs {
		harnessStderr = stderr
	}
	opts := harness.Options{CodexBin: *codexBin, Stderr: harnessStderr, Logf: logf, ACP: map[string]harness.ACPAgent{}}
	if *codexHome != "" {
		opts.CodexEnv = map[string]string{"CODEX_HOME": *codexHome}
	}
	if *gooseBin != "" {
		opts.ACP["goose"] = harness.ACPAgent{Bin: *gooseBin}
	}
	if *acpBin != "" {
		opts.ACP[*h] = harness.ACPAgent{Bin: *acpBin, Args: strings.Fields(*acpArgs)}
	}
	if !harness.TakesImages(*h) {
		fmt.Fprintf(stderr, "%s cannot take images (its headless input is text only)\n", *h)
		return 2
	}
	dir := *cwd
	if dir == "" {
		d, err := os.MkdirTemp("", "swk-vision-probe-")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer os.RemoveAll(d)
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	spec := adapter.StartSpec{Name: "dev/vision-probe", Cwd: abs, Model: *modelName, Provider: *provider,
		ApprovalPolicy: "on-request", Sandbox: "read-only"}
	if *save != "" {
		logf("the probe image is written to %s", *save)
	}
	res, err := probeAndSave(ctx, opts, *h, spec, *save, logf)
	return printProbe(stdout, stderr, res, err)
}

func probeAndSave(ctx context.Context, opts harness.Options, name string, spec adapter.StartSpec, save string, logf func(string, ...any)) (ProbeResult, error) {
	res, err := Probe(ctx, opts, name, spec, logf)
	if save != "" && res.Code != "" {
		if img, perr := ProbePNG(res.Code); perr == nil {
			if werr := os.WriteFile(save, img, 0o644); werr != nil && err == nil {
				err = werr
			}
		}
	}
	return res, err
}

func printProbe(stdout, stderr io.Writer, res ProbeResult, err error) int {
	who := res.Harness
	if res.Model != "" {
		who += ", model " + res.Model
	}
	if res.Provider != "" {
		who += ", provider " + res.Provider
	}
	if err != nil {
		fmt.Fprintf(stdout, "FAIL  %s: %v\n", who, err)
		if errors.Is(err, adapter.ErrNoImages) {
			fmt.Fprintln(stdout, "      (the harness refused the image before sending it to a model)")
		}
		return 1
	}
	verdict := "FAIL"
	if res.Pass {
		verdict = "PASS"
	}
	fmt.Fprintf(stdout, "%s  %s\n      code %s, answer %q (%.1fs)\n", verdict, who, res.Code, strings.TrimSpace(res.Answer), res.Duration.Seconds())
	if !res.Pass {
		fmt.Fprintln(stdout, "      the model did not read the code: it may not take images, or the provider endpoint may drop them")
		return 1
	}
	return 0
}
