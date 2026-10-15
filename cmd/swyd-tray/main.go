// SPDX-License-Identifier: GPL-3.0-or-later

// Command swyd-tray shows the node's status in the system tray on Linux and
// Windows, with a menu to open the web UI, start, stop or restart the user
// services, open the logs and config folders, and start the tray at login.
// It is a separate binary so that swyd itself stays a plain command-line
// program; on macOS the Switchyard menu-bar app does the same job.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/jm2/switchyard/internal/tray"
	"github.com/jm2/switchyard/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("%s-tray %s (%s %s/%s)\n", version.Bin, version.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return
		default:
			fmt.Fprintf(os.Stderr, "usage: %s-tray [version]\n", version.Bin)
			os.Exit(2)
		}
	}
	b, err := newBackend()
	if err != nil {
		fail(err)
	}
	if err := tray.NewApp(b).Run(); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fail(err)
	}
}

func fail(err error) {
	reportFatal(err)
	os.Exit(1)
}

// stderr is where diagnostics go; the Windows build has no console, so it
// shows a message box instead.
var stderr io.Writer = os.Stderr
