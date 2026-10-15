// SPDX-License-Identifier: GPL-3.0-or-later

// Package ciannotate turns test failures into GitHub Actions annotations,
// so they can be read through the API without downloading the job log.
// Test packages import it for its side effect:
//
//	import _ "github.com/jm2/switchyard/internal/ciannotate"
//
// It does nothing outside GitHub Actions. Under Actions it registers a
// problem matcher (build-aux/ci/go-test-matcher.json) by printing the
// workflow command to stderr, which `go test` shows when a package fails.
// It prints once per process tree, so test binaries that re-run themselves
// as fake harnesses stay quiet.
package ciannotate

import (
	"fmt"
	"os"
	"path/filepath"
)

const doneEnv = "SWYD_CI_MATCHER"

func init() {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv(doneEnv) != "" {
		return
	}
	_ = os.Setenv(doneEnv, "1")
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		m := filepath.Join(dir, "build-aux", "ci", "go-test-matcher.json")
		if _, err := os.Stat(m); err == nil {
			fmt.Fprintf(os.Stderr, "::add-matcher::%s\n", m)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}
