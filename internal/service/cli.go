// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const usage = `usage: swyd service install [--no-start] [node|coordinator]
       swyd service uninstall [node|coordinator]
       swyd service start|stop|restart [--if-installed] [node|coordinator]
       swyd service status [--json] [node|coordinator]

Registers the node (the default) or the coordinator with your own service
manager, to run as you: a systemd user unit on Linux, a LaunchAgent on
macOS, a logon task on Windows. Nothing runs as root or SYSTEM.`

// Main implements `swyd service ...`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("service "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	var noStart, asJSON, ifInstalled bool
	var exe string
	switch verb {
	case "install":
		fs.BoolVar(&noStart, "no-start", false, "register only; start at the next login")
		fs.StringVar(&exe, "swyd", "", "swyd binary the service runs (default: this one)")
	case "status":
		fs.BoolVar(&asJSON, "json", false, "print JSON (what the tray apps read)")
	case "start", "stop", "restart":
		fs.BoolVar(&ifInstalled, "if-installed", false, "do nothing, successfully, when the service is not installed")
	case "uninstall":
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown service command %q\n%s\n", verb, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	role := fs.Arg(0)
	if verb == "status" {
		return status(role, asJSON, stdout, stderr)
	}
	if role == "" {
		role = Node
	}
	if err := checkRole(role); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var err error
	switch verb {
	case "install":
		if !Query(role).Configured {
			hint := "swyd node enroll <token>"
			if role == Coordinator {
				hint = "swyd coordinator init --host <name>"
			}
			fmt.Fprintf(stdout, "Note: %s is not set up yet (%s); the service will fail until it is.\n", role, hint)
		}
		err = Install(role, exe, noStart, stdout)
	case "uninstall":
		err = Uninstall(role, stdout)
	case "start":
		err = Start(role)
	case "stop":
		err = Stop(role)
	case "restart":
		err = Restart(role)
	}
	if ifInstalled && errors.Is(err, ErrNotInstalled) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func status(role string, asJSON bool, stdout, stderr io.Writer) int {
	if role != "" {
		if err := checkRole(role); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	o := Status()
	if asJSON {
		var v any = o
		switch role {
		case Node:
			v = o.Node
		case Coordinator:
			v = o.Coordinator
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	for _, in := range []Info{o.Node, o.Coordinator} {
		if role != "" && in.Role != role {
			continue
		}
		fmt.Fprintf(stdout, "%-12s %s\n", in.Role, Describe(in))
		if d := detail(in); d != "" {
			fmt.Fprintf(stdout, "%-12s %s\n", "", d)
		}
	}
	if o.WebUI != "" && role == "" {
		fmt.Fprintf(stdout, "%-12s %s\n", "web UI", o.WebUI)
	}
	return 0
}

// Describe is a one-line summary of a role, as the CLI and tray show it.
func Describe(in Info) string {
	var parts []string
	switch {
	case !in.Configured && in.Role == Node:
		parts = append(parts, "not enrolled")
	case !in.Configured:
		parts = append(parts, "not set up")
	case in.Running && in.Role == Node && in.Status != nil && in.Status.Connected:
		parts = append(parts, "running, connected")
	case in.Running && in.Role == Node && in.Status != nil:
		parts = append(parts, "running, not connected")
	case in.Running:
		parts = append(parts, "running")
	default:
		parts = append(parts, "stopped")
	}
	switch {
	case in.Installed && in.Enabled:
		parts = append(parts, in.Manager+" "+in.Unit+" (starts at login)")
	case in.Installed:
		parts = append(parts, in.Manager+" "+in.Unit)
	case in.Configured:
		parts = append(parts, "no service (swyd service install "+in.Role+")")
	}
	if in.Error != "" {
		parts = append(parts, "service manager: "+in.Error)
	}
	return strings.Join(parts, "; ")
}

func detail(in Info) string {
	s := in.Status
	if s == nil {
		return ""
	}
	if in.Role == Node {
		if s.Connected {
			return fmt.Sprintf("%s, connected to %s since %s", s.Host, s.Coordinator, s.ConnectedSince.Local().Format(time.DateTime))
		}
		if s.LastError != "" {
			return fmt.Sprintf("%s, reconnecting to %s: %s", s.Host, s.Coordinator, s.LastError)
		}
		return fmt.Sprintf("%s, connecting to %s", s.Host, s.Coordinator)
	}
	return "web UI " + s.WebUI
}
