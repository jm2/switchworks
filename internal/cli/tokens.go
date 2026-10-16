// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/render"
)

func init() { commands["token"] = cmdToken }

const tokenUsage = `usage:
  swyd token create <name> --scope read|operate|admin [--ttl 720h] [--cli-config [--api URL]]
  swyd token ls [--json]
  swyd token rm <name-or-id>`

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
	case "rm", "remove", "revoke":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: swyd token rm <name-or-id>")
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

func cmdTokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "", "read (look only), operate (read, plus send, approve, interrupt, stop, spawn) or admin (everything)")
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h (default: until revoked)")
	cliConfig := fs.Bool("cli-config", false, "print a complete cli.json with the new token, for another machine")
	api := fs.String("api", "", "with --cli-config: the API URL that machine dials (default: this CLI's)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: swyd token create <name> --scope read|operate|admin [--ttl 720h] [--cli-config [--api URL]]")
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
		fmt.Fprintln(stderr, "swyd token create: --ttl must be 0 (no expiry) or at least 1s")
		return 2
	}
	if *api != "" && !*cliConfig {
		fmt.Fprintln(stderr, "swyd token create: --api only applies with --cli-config")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	t, err := c.CreateToken(ctx, pos[0], *scope, *ttl)
	if err != nil {
		return fail(stderr, err)
	}
	expires := "never expires"
	if !t.Expires.IsZero() {
		expires = "expires " + t.Expires.Local().Format(time.RFC3339)
	}
	fmt.Fprintf(stderr, "created token %s (%s scope, %s, id %s)\n", t.Name, t.Scope, expires, t.ID)
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
		fmt.Fprintln(stderr, "This cli.json holds the token's secret: save it as ~/.config/swyd/cli.json (chmod 600) on that machine.")
		return 0
	}
	fmt.Fprintln(stdout, t.Secret)
	fmt.Fprintln(stderr, "The secret is shown only now. Revoke it with: swyd token rm "+t.Name)
	return 0
}

func cmdTokenList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: swyd token ls [--json]")
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
	fmt.Fprintln(tw, "NAME\tSCOPE\tCREATED\tLAST USED\tEXPIRES\tID")
	for _, t := range toks {
		exp := "never"
		if !t.Expires.IsZero() {
			exp = t.Expires.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scope, t.Created.Local().Format("2006-01-02"), ago(t.LastUsed), exp, t.ID)
	}
	tw.Flush()
	return 0
}
