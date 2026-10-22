// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	attachpkg "github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/render"
)

func init() {
	commands["msg"] = cmdMsg
	commands["ask"] = cmdAsk
	commands["reply"] = cmdReply
	commands["inbox"] = cmdInbox
	commands["messages"] = cmdMessages
	commands["usage"] = cmdUsage
	commands["ui-link"] = cmdUILink
}

func cmdUILink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui-link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "", "limit the browser to read or operate (default: this CLI token's scope)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: swk ui-link [--scope read|operate|admin]")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	l, err := c.UILink(ctx, *scope)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, l.URL)
	scopeNote := ""
	if l.Scope != "" {
		scopeNote = ", " + l.Scope + " scope"
	}
	fmt.Fprintf(stderr, "(signs one browser in%s; works once, until %s)\n", scopeNote, l.ExpiresAt.Local().Format("15:04"))
	return 0
}

func printSent(stdout io.Writer, res fleet.SendResult) {
	for _, m := range res.Messages {
		state := m.State
		if state == model.MsgPending {
			state = "pending (host offline; delivered when it returns)"
		}
		if m.State == fleet.Refused {
			fmt.Fprintf(stdout, "- → %s: not sent\n", m.To)
		} else {
			fmt.Fprintf(stdout, "%s → %s: %s\n", m.ID, orDash(m.Target), state)
		}
		if m.Error != "" {
			fmt.Fprintf(stdout, "  %s\n", m.Error)
		}
	}
}

func cmdMsg(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("msg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "auto (default: into the running turn if there is one), queue, steer or interrupt")
	bcast := fs.Bool("broadcast", false, "send to every session matching an @selector")
	from := fs.String("from", "", "sender (default user:cli)")
	ttl := fs.Duration("ttl", 0, "expire if undelivered after this long (default 24h)")
	images := addImageFlag(fs)
	var to imageFlag
	fs.Var(&to, "to", "a recipient address (repeat to send to several, up to 64; then every argument is text)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: swk msg [flags] [--image FILE]... <address> <text>\n       swk msg [flags] --to <address> [--to <address>]... <text>\n  address: host/name, name, @key=value (labels, host, harness) or user:<name>")
		fs.PrintDefaults()
	}
	pos, err := interleave(fs, args)
	need := 2 // address and text
	if len(to) > 0 {
		need = 1
	}
	if err != nil || len(pos) < need {
		fs.Usage()
		return 2
	}
	if len(to) == 0 {
		to, pos = imageFlag{pos[0]}, pos[1:]
	}
	pm := apiclient.PostMessage{From: *from, Text: strings.Join(pos, " "), Mode: *mode, TTLSeconds: int(ttl.Seconds())}
	if len(to) == 1 {
		pm.To = to[0]
	} else {
		pm.ToList = to
	}
	atts, err := readImages(*images)
	if err != nil {
		return fail(stderr, err)
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	kind := model.MsgNotify
	if *bcast {
		kind = model.MsgBroadcast
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	pm.Kind, pm.Attachments = kind, atts
	res, err := c.PostMessage(ctx, pm)
	if err != nil {
		return fail(stderr, err)
	}
	printSent(stdout, res)
	return 0
}

func cmdAsk(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the answer (0: print the ticket and exit)")
	from := fs.String("from", "", "sender (default user:cli)")
	images := addImageFlag(fs)
	pos, err := interleave(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: swk ask [--wait 10m] [--image FILE]... <address> <question>")
		return 2
	}
	atts, err := readImages(*images)
	if err != nil {
		return fail(stderr, err)
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	if *wait > fleet.MaxAskWait {
		*wait = fleet.MaxAskWait
	}
	secs := int(wait.Seconds())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *wait+time.Minute)
	defer cancel2()
	res, err := c.Ask(ctx, apiclient.PostMessage{From: *from, To: pos[0], Text: strings.Join(pos[1:], " "), WaitSeconds: &secs, Attachments: atts})
	if err != nil {
		return fail(stderr, err)
	}
	switch res.Status {
	case fleet.AskReplied:
		fmt.Fprintf(stderr, "── %s answered:\n", res.ReplyFrom)
		fmt.Fprintln(stdout, res.Reply)
		return 0
	case fleet.AskFailed:
		return fail(stderr, fmt.Errorf("ask %s: %s", res.MessageID, res.Error))
	}
	fmt.Fprintf(stdout, "no answer yet from %s; ticket %s (see `swk inbox`)\n", orDash(res.Target), res.MessageID)
	return 0
}

func cmdReply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "sender (default user:cli)")
	pos, err := interleave(fs, args)
	if err != nil || len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: swk reply <ask-id> <answer>")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	res, err := c.Reply(ctx, pos[0], strings.Join(pos[1:], " "), *from)
	if err != nil {
		return fail(stderr, err)
	}
	how := res.State
	if res.Inline {
		how = "handed to the waiting asker"
	}
	fmt.Fprintf(stdout, "reply %s: %s\n", res.ReplyID, how)
	return 0
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

// attachmentsLabel names a message's images and their sizes, like
// "[test.png 589 KiB, b.jpg 1.2 MiB]".
func attachmentsLabel(atts []model.Attachment) string {
	parts := make([]string, len(atts))
	for i, a := range atts {
		name := a.Name
		if name == "" {
			name = "image"
		}
		parts[i] = name + " " + attachpkg.HumanSize(a.Size)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func printMessages(stdout io.Writer, msgs []model.Message) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGE\tKIND\tFROM\tTO\tSTATE\tTEXT\tID")
	for _, m := range msgs {
		to := m.ToPath
		if to == "" {
			to = m.To
		}
		state := m.State
		if m.Inline {
			state += " (inline)"
		}
		// The 60-rune limit is the text's; the attachments are shown whole.
		text := oneLine(m.Text, 60)
		if len(m.Attachments) > 0 {
			text = strings.TrimSpace(oneLine(attachmentsLabel(m.Attachments), 1<<16) + " " + text)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ago(m.CreatedAt), m.Kind, m.From, to, state, text, m.ID)
	}
	tw.Flush()
}

func cmdInbox(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("n", 30, "how many messages")
	asJSON := fs.Bool("json", false, "print JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	msgs, err := c.Messages(ctx, apiclient.MessageQuery{Inbox: true, Limit: *limit})
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		_ = json.NewEncoder(render.JSONSafe(stdout)).Encode(msgs)
		return 0
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "inbox is empty")
		return 0
	}
	open := 0
	for _, m := range msgs {
		if m.Kind == model.MsgAsk && m.State != model.MsgAnswered && m.State != model.MsgExpired && m.State != model.MsgFailed {
			open++
			fmt.Fprintf(stdout, "? %s asks (%s ago) [%s]:\n", m.From, ago(m.CreatedAt), m.ID)
			for _, line := range strings.Split(m.Text, "\n") {
				fmt.Fprintf(stdout, "    %s\n", line)
			}
		}
	}
	if open > 0 {
		fmt.Fprintf(stdout, "answer with: swk reply <id> <answer>\n\n")
	}
	printMessages(stdout, msgs)
	return 0
}

func cmdMessages(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("messages", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("n", 50, "how many messages")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := interleave(fs, args)
	if err != nil || len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: swk messages [-n 50] [--json] [session]")
		return 2
	}
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	q := apiclient.MessageQuery{Limit: *limit}
	if len(pos) == 1 {
		s, ok := resolve(c, pos[0], stderr)
		if !ok {
			return 1
		}
		q.Session = s.ID
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	msgs, err := c.Messages(ctx, q)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		_ = json.NewEncoder(render.JSONSafe(stdout)).Encode(msgs)
		return 0
	}
	printMessages(stdout, msgs)
	return 0
}

func cmdUsage(args []string, stdout, stderr io.Writer) int {
	c, ok := client(stderr)
	if !ok {
		return 1
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	u, err := c.Usage(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	budget := ""
	if u.MaxDailyTokens > 0 {
		budget = fmt.Sprintf(" of %s budget", human(u.MaxDailyTokens))
	}
	fmt.Fprintf(stdout, "today (%s UTC): %s tokens%s\n\n", u.Today, human(u.TodayTokens), budget)
	table := func(title string, m map[string]model.Usage) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\tINPUT\tCACHED\tOUTPUT\tTOTAL\n", title)
		for _, k := range keys {
			v := m[k]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k, human(v.InputTokens), human(v.CachedInputTokens), human(v.OutputTokens), human(v.TotalTokens))
		}
		tw.Flush()
		fmt.Fprintln(stdout)
	}
	table("HOST", u.ByHost)
	table("MODEL", u.ByModel)
	days := make([]string, 0, len(u.Daily))
	for d := range u.Daily {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	for _, d := range days {
		fmt.Fprintf(stdout, "%s  %s\n", d, human(u.Daily[d]))
	}
	return 0
}
