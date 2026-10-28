// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/render"
)

func init() { commands["reminders"] = cmdReminders }

// cmdReminders lists the reminders a session set itself with fleet_remind
// (swk reminders <session>), or cancels one (swk reminders cancel
// <session> <id>).
func cmdReminders(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: swk reminders [--json] <session>\n       swk reminders cancel <session> <id>")
		return 2
	}
	cancelling := len(args) == 3 && args[0] == "cancel"
	if cancelling {
		args = args[1:]
	}
	fs := flag.NewFlagSet("reminders", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := interleave(fs, args)
	if err != nil || (!cancelling && len(pos) != 1) || (cancelling && len(pos) != 2) {
		return usage()
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	s, ok := resolve(c, pos[0], stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	if cancelling {
		res, err := c.CancelReminder(ctx, s.ID, pos[1])
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s: reminder %s cancelled\n", s.Path, res.ID)
		return 0
	}
	rs, err := c.Reminders(ctx, s.ID)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		_ = json.NewEncoder(render.JSONSafe(stdout)).Encode(rs)
		return 0
	}
	printReminders(stdout, s.Path, rs.Reminders)
	return 0
}

// printReminders prints a session's pending reminders as a table.
func printReminders(w io.Writer, path string, rs []fleet.Reminder) {
	if len(rs) == 0 {
		fmt.Fprintf(w, "%s has no pending reminders\n", path)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDUE\tIN\tTEXT")
	for _, r := range rs {
		// The text as the agent wrote it: the line after the header.
		text := r.Text
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			text = rest
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if t := []rune(text); len(t) > 80 {
			text = string(t[:77]) + "..."
		}
		in := time.Until(r.DueAt).Round(time.Second)
		if in < 0 {
			in = 0
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.DueAt.Local().Format("Jan 2 15:04"), in, text)
	}
	_ = tw.Flush()
}
