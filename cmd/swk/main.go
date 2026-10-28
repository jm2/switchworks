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
  swk policy <session> <policy>  change a running session's approval policy
                                 (untrusted, on-request, never; from its next turn)
  swk approve <session> <approval-id> <decision>
  swk stop <session>             stop managing a session
  swk rm <session>               delete a stopped session's record and events
                                 (admin; ended ones go after 90 days anyway)
  swk events <session>           print a session's event log as JSON

Messages:
  swk msg <address> <text>       message a session, an @label selector or a person
  swk ask <address> <question>   ask and wait for the answer
  swk inbox                      messages and open questions for people
  swk reply <ask-id> <answer>    answer an agent's question
  swk messages dismiss <id>      clear a question without an answer
  swk messages read <id>         mark a message to a person read
  swk messages [session]         the fleet's message traffic
  swk messages promote|cancel <id>
                                 send a queued message now, or cancel it
  swk reminders <session>        reminders a session set itself (fleet_remind)
  swk reminders cancel <session> <id>
                                 cancel one (owners)
  swk usage                      token use by host, model and day
  swk locks [prefix]             resource locks agents hold (fleet_lock)
  swk locks break <name>         end a lock's lease (owner or admin)
  swk ui-link [--scope S]        print a one-time sign-in link for the web UI

Access:
  swk token create <name> --scope read|operate|admin [--owner] [--ttl 720h] [--cli-config]
                                 make a named API token (secret shown once);
                                 --cli-config prints a cli.json for another machine;
                                 --owner: its messages reach agents as yours (keep it
                                 off every host where agents run)
  swk token owner <name> on|off  mark a token as the owner's, or not
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
  swk service install --system coordinator
                                 run the coordinator at boot as an account of
                                 its own (Linux, Windows; root or administrator)
  swk --system <command>         run a coordinator or CLI command against that
                                 coordinator, as its account

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
	if args[0] == "--system" {
		code, done := service.EnterSystem(args[1:], stderr)
		if done {
			return code
		}
		args = args[1:]
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
