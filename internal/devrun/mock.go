// SPDX-License-Identifier: GPL-3.0-or-later

package devrun

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/jm2/switchworks/internal/codex/codextest"
)

// MockMain runs `swk dev mock-provider`: a local fake model endpoint plus a
// CODEX_HOME that points Codex at it, for smoke tests that spend no tokens.
func MockMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev mock-provider", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("codex-home", "", "directory to create as CODEX_HOME (default: ./mock-codex-home)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *home == "" {
		*home = "mock-codex-home"
	}
	abs, err := filepath.Abs(*home)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := codextest.StartMockProvider()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer m.Close()
	env, err := m.WriteCodexHome(abs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "mock model provider at %s\nCODEX_HOME written to %s\n\nUse it with:\n", m.URL, abs)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(stdout, "  export %s=%q\n", k, env[k])
	}
	fmt.Fprintf(stdout, "  swk dev run --codex-home %q \"hello\"\n  (\"RUN: <cmd>\" makes the mock call a shell tool)\n", abs)
	fmt.Fprintf(stdout, "  swk dev vision-probe --codex-home %q\n  (the mock reads the probe's code from the PNG's metadata, so this checks the image path, not a model)\n", abs)
	fmt.Fprintf(stdout, "\nAnthropic Messages API (goose: GOOSE_PROVIDER=anthropic ANTHROPIC_API_KEY=mock):\n  export ANTHROPIC_HOST=%q\n\nCtrl-C to stop.\n", m.AnthropicHost())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return 0
}
