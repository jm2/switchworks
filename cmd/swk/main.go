// SPDX-License-Identifier: GPL-3.0-or-later

// Command swk is Switchworks's single binary: coordinator, node, CLI and dev
// tools are subcommands.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/jm2/switchworks/internal/cli"
	"github.com/jm2/switchworks/internal/coord"
	"github.com/jm2/switchworks/internal/devrun"
	"github.com/jm2/switchworks/internal/mcp"
	"github.com/jm2/switchworks/internal/node"
	"github.com/jm2/switchworks/internal/service"
	"github.com/jm2/switchworks/internal/version"
)

const usage = `swk — %s %s: fleet control plane for coding agents

Fleet:
  swk ls                         list sessions on every host
  swk hosts                      list enrolled hosts
  swk hosts rm <host>            revoke a host (its certificate stops working)
  swk spawn <host> [flags] <prompt>
                                 start a managed session on a host
  swk attach <session>           stream a session live (host/name or id)
  swk send <session> <text>      queue a message into a session (--steer, --interrupt)
  swk interrupt <session>        cancel a session's active turn
  swk approve <session> <approval-id> <decision>
  swk stop <session>             stop managing a session
  swk events <session>           print a session's event log as JSON

Messages:
  swk msg <address> <text>       message a session, an @label selector or a person
  swk ask <address> <question>   ask and wait for the answer
  swk inbox                      messages and open questions for people
  swk reply <ask-id> <answer>    answer an agent's question
  swk messages [session]         the fleet's message traffic
  swk usage                      token use by host, model and day
  swk ui-link [--scope S]        print a one-time sign-in link for the web UI

Access:
  swk token create <name> --scope read|operate|admin [--ttl 720h] [--cli-config]
                                 make a named API token (secret shown once);
                                 --cli-config prints a cli.json for another machine
  swk token ls                   list API tokens
  swk token rm <name-or-id>      revoke a token and the web sessions it signed in

Services:
  swk coordinator init [flags]   create the CA, server certificate and admin token
  swk coordinator run [flags]    run the coordinator
  swk coordinator enroll-token <host>
                                 print a one-time token to enroll a host
  swk coordinator cli-config     print the admin CLI config (admin token: keep it off agent hosts)
  swk coordinator health         check the coordinator answers (a container's healthcheck)
  swk node enroll -              enroll this host with a coordinator (token on stdin)
  swk node run [flags]           run the node service
  swk service install [node|coordinator]
                                 run it at login as you (systemd user unit,
                                 LaunchAgent or logon task) and start it now
  swk service start|stop|restart|status|uninstall [node|coordinator]

Development:
  swk mcp                        MCP server with the fleet tools (started by the node inside sessions)
  swk dev run [flags] <prompt>   run one turn directly against a harness
  swk dev vision-probe [flags]   check that a harness and model can read an image
  swk dev mock-provider          serve a fake model endpoint for token-free smoke tests

  swk version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, version.Name, version.Version)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "%s %s (%s %s/%s)\n", version.Bin, version.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "--help", "-h":
		fmt.Fprintf(stdout, usage, version.Name, version.Version)
		return 0
	case "dev":
		if len(args) >= 2 && args[1] == "run" {
			return devrun.Main(args[2:], stdout, stderr)
		}
		if len(args) >= 2 && args[1] == "mock-provider" {
			return devrun.MockMain(args[2:], stdout, stderr)
		}
		if len(args) >= 2 && args[1] == "vision-probe" {
			return devrun.VisionMain(args[2:], stdout, stderr)
		}
		fmt.Fprintln(stderr, "usage: swk dev run [flags] <prompt> | swk dev vision-probe [flags] | swk dev mock-provider [--codex-home DIR]")
		return 2
	case "mcp":
		return mcp.Main(os.Stdin, stdout, stderr)
	case "coordinator":
		return coord.Main(args[1:], stdout, stderr)
	case "node":
		return node.Main(args[1:], stdout, stderr)
	case "service":
		return service.Main(args[1:], stdout, stderr)
	default:
		return cli.Main(args, stdout, stderr)
	}
}
