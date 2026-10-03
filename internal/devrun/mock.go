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

	"github.com/jm2/switchyard/internal/codex/codextest"
)

// MockMain runs `swyd dev mock-provider`: a local fake model endpoint plus a
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
	fmt.Fprintf(stdout, "  swyd dev run --codex-home %q \"hello\"\n  (\"RUN: <cmd>\" makes the mock call a shell tool)\n\nCtrl-C to stop.\n", abs)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return 0
}
