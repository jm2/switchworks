// SPDX-License-Identifier: GPL-3.0-or-later

// Command swyd is Switchyard's single binary: coordinator, node, CLI and dev
// tools are subcommands.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/jm2/switchyard/internal/cli"
	"github.com/jm2/switchyard/internal/coord"
	"github.com/jm2/switchyard/internal/devrun"
	"github.com/jm2/switchyard/internal/mcp"
	"github.com/jm2/switchyard/internal/node"
	"github.com/jm2/switchyard/internal/service"
	"github.com/jm2/switchyard/internal/version"
)

const usage = `swyd — %s %s: fleet control plane for coding agents

Fleet:
  swyd ls                         list sessions on every host
  swyd hosts                      list enrolled hosts
  swyd hosts rm <host>            revoke a host (its certificate stops working)
  swyd spawn <host> [flags] <prompt>
                                  start a managed session on a host
  swyd attach <session>           stream a session live (host/name or id)
  swyd send <session> <text>      queue a message into a session (--steer, --interrupt)
  swyd interrupt <session>        cancel a session's active turn
  swyd approve <session> <approval-id> <decision>
  swyd stop <session>             stop managing a session
  swyd events <session>           print a session's event log as JSON

Messages:
  swyd msg <address> <text>       message a session, an @label selector or a person
  swyd ask <address> <question>   ask and wait for the answer
  swyd inbox                      messages and open questions for people
  swyd reply <ask-id> <answer>    answer an agent's question
  swyd messages [session]         the fleet's message traffic
  swyd usage                      token use by host, model and day
  swyd ui-link [--scope S]        print a one-time sign-in link for the web UI

Access:
  swyd token create <name> --scope read|operate|admin [--ttl 720h] [--cli-config]
                                  make a named API token (secret shown once);
                                  --cli-config prints a cli.json for another machine
  swyd token ls                   list API tokens
  swyd token rm <name-or-id>      revoke a token and the web sessions it signed in

Services:
  swyd coordinator init [flags]   create the CA, server certificate and admin token
  swyd coordinator run [flags]    run the coordinator
  swyd coordinator enroll-token <host>
                                  print a one-time token to enroll a host
  swyd coordinator cli-config     print the admin CLI config (admin token: keep it off agent hosts)
  swyd node enroll -              enroll this host with a coordinator (token on stdin)
  swyd node run [flags]           run the node service
  swyd service install [node|coordinator]
                                  run it at login as you (systemd user unit,
                                  LaunchAgent or logon task) and start it now
  swyd service start|stop|restart|status|uninstall [node|coordinator]

Development:
  swyd mcp                        MCP server with the fleet tools (started by the node inside sessions)
  swyd dev run [flags] <prompt>   run one turn directly against a harness
  swyd dev vision-probe [flags]   check that a harness and model can read an image
  swyd dev mock-provider          serve a fake model endpoint for token-free smoke tests

  swyd version
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
		fmt.Fprintln(stderr, "usage: swyd dev run [flags] <prompt> | swyd dev vision-probe [flags] | swyd dev mock-provider [--codex-home DIR]")
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
