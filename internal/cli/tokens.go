// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/render"
)

func init() { commands["token"] = cmdToken }

const tokenUsage = `usage:
  swk token create <name> --scope read|operate|admin [--owner] [--ttl 720h] [--cli-config [--api URL]]
  swk token owner <name-or-id> on|off
  swk token ls [--json]
  swk token rm <name-or-id>`

func cmdToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, tokenUsage)
		return 2
	}
	switch args[0] {
	case "create", "new":
		return cmdTokenCreate(args[1:], stdout, stderr)
	case "ls", "list":
		return cmdTokenList(args[1:], stdout, stderr)
	case "owner":
		if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
			fmt.Fprintln(stderr, "usage: swk token owner <name-or-id> on|off\n  "+ownerHelp)
			return 2
		}
		c, ok := client(stderr)
		if !ok {
			return 1
		}
		ctx, cancel := ctxTimeout()
		defer cancel()
		t, err := c.SetTokenOwner(ctx, args[1], args[2] == "on")
		if err != nil {
			return fail(stderr, err)
		}
		if t.Owner {
			fmt.Fprintf(stdout, "%s is an owner token: its messages reach agents as yours. Keep it off every host where agents run.\n", t.Name)
		} else {
			fmt.Fprintf(stdout, "%s is not an owner token: its messages reach agents with a header naming it.\n", t.Name)
		}
		return 0
	case "rm", "remove", "revoke":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: swk token rm <name-or-id>")
			return 2
		}
		c, ok := client(stderr)
		if !ok {
			return 1
		}
		ctx, cancel := ctxTimeout()
		defer cancel()
		if err := c.DeleteToken(ctx, args[1]); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "revoked %s; web sessions signed in with it are signed out\n", args[1])
		return 0
	}
	fmt.Fprintln(stderr, tokenUsage)
	return 2
}

const ownerHelp = "an owner token's messages (and those of browsers it signs in) reach agents as yours, without a header; never put one on a host where agents run"

func cmdTokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "", "read (look only), operate (read, plus send, approve, interrupt, stop, spawn) or admin (everything)")
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h (default: until revoked)")
	owner := fs.Bool("owner", false, ownerHelp)
	cliConfig := fs.Bool("cli-config", false, "print a complete cli.json with the new token, for another machine")
	api := fs.String("api", "", "with --cli-config: the API URL that machine dials (default: this CLI's)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: swk token create <name> --scope read|operate|admin [--owner] [--ttl 720h] [--cli-config [--api URL]]")
		fs.PrintDefaults()
	}
	pos, err := interleave(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || *scope == "" {
		fs.Usage()
		return 2
	}
	if *ttl < 0 || (*ttl > 0 && *ttl < time.Second) {
		// The API counts whole seconds, where 0 means no expiry.
		fmt.Fprintln(stderr, "swk token create: --ttl must be 0 (no expiry) or at least 1s")
		return 2
	}
	if *api != "" && !*cliConfig {
		fmt.Fprintln(stderr, "swk token create: --api only applies with --cli-config")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	t, err := c.CreateTokenWith(ctx, apiclient.CreateTokenRequest{Name: pos[0], Scope: *scope, TTLSeconds: int64(*ttl / time.Second), Owner: *owner})
	if err != nil {
		return fail(stderr, err)
	}
	expires := "never expires"
	if !t.Expires.IsZero() {
		expires = "expires " + t.Expires.Local().Format(time.RFC3339)
	}
	kind := ""
	if t.Owner {
		kind = ", owner"
	}
	fmt.Fprintf(stderr, "created token %s (%s scope%s, %s, id %s)\n", t.Name, t.Scope, kind, expires, t.ID)
	if *cliConfig {
		cfg := c.Config()
		cfg.Token = t.Secret
		if *api != "" {
			cfg.API = *api
		}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s\n", b)
		fmt.Fprintln(stderr, "This cli.json holds the token's secret: save it as ~/.config/swk/cli.json (chmod 600) on that machine.")
		return 0
	}
	fmt.Fprintln(stdout, t.Secret)
	fmt.Fprintln(stderr, "The secret is shown only now. Revoke it with: swk token rm "+t.Name)
	return 0
}

func cmdTokenList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: swk token ls [--json]")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	toks, err := c.Tokens(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if toks == nil {
			toks = []apiclient.TokenInfo{}
		}
		_ = json.NewEncoder(render.JSONSafe(stdout)).Encode(toks)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCOPE\tOWNER\tCREATED\tLAST USED\tEXPIRES\tID")
	for _, t := range toks {
		exp := "never"
		if !t.Expires.IsZero() {
			exp = t.Expires.Local().Format("2006-01-02 15:04")
		}
		owner := "-"
		if t.Owner {
			owner = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scope, owner, t.Created.Local().Format("2006-01-02"), ago(t.LastUsed), exp, t.ID)
	}
	tw.Flush()
	return 0
}
