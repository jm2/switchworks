// SPDX-License-Identifier: GPL-3.0-or-later

package acpad

import (
	"fmt"
	"strconv"
	"strings"
)

// unifiedDiff renders a unified diff (three lines of context) between an
// edit's old and new text. ACP reports edits as whole old/new texts; this
// turns them into the same form Codex's turn diffs use. Very large files
// fall back to a whole-file replacement.
//
// The model picks the path, so one with a newline or another unprintable
// character is quoted, as git does, rather than written raw, where it
// could add lines of its own to the diff's header.
func unifiedDiff(path string, oldText *string, newText string) string {
	rel := strings.TrimPrefix(path, "/")
	from, to := printable("a/"+rel), printable("b/"+rel)
	var a []string
	if oldText == nil {
		from = "/dev/null"
	} else {
		a = splitLines(*oldText)
	}
	b := splitLines(newText)
	ops := diffLines(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	const ctx = 3
	i := 0
	for i < len(ops) {
		if ops[i].op == ' ' {
			i++
			continue
		}
		// A hunk spans changes separated by at most 2*ctx equal lines.
		start := i - ctx
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(ops) {
			if ops[end].op != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].op == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end += min(ctx, run-end)
				break
			}
			end = run
		}
		aStart, bStart, aLen, bLen := ops[start].a, ops[start].b, 0, 0
		var body strings.Builder
		for _, o := range ops[start:end] {
			switch o.op {
			case ' ':
				aLen++
				bLen++
			case '-':
				aLen++
			case '+':
				bLen++
			}
			body.WriteByte(o.op)
			body.WriteString(o.text)
			body.WriteByte('\n')
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		out.WriteString(body.String())
		i = end
	}
	return out.String()
}

// printable returns s as it is when every character in it prints, and
// quoted, with escapes, when not: newlines, other control characters and
// invisible formatting such as bidirectional overrides.
func printable(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type diffOp struct {
	op   byte // ' ', '-', '+'
	text string
	a, b int // positions in a and b before this line
}

// diffLines is an LCS line diff, bounded to keep memory in check.
func diffLines(a, b []string) []diffOp {
	// Trim the common prefix and suffix first; edits are usually local.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{' ', a[i], i, i})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > 4_000_000 {
		for i, l := range ma {
			ops = append(ops, diffOp{'-', l, pre + i, pre})
		}
		for j, l := range mb {
			ops = append(ops, diffOp{'+', l, pre + len(ma), pre + j})
		}
	} else {
		n, m := len(ma), len(mb)
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else if lcs[i+1][j] >= lcs[i][j+1] {
					lcs[i][j] = lcs[i+1][j]
				} else {
					lcs[i][j] = lcs[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, diffOp{' ', ma[i], pre + i, pre + j})
				i++
				j++
			case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, diffOp{'-', ma[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, diffOp{'+', mb[j], pre + i, pre + j})
				j++
			}
		}
	}
	for k := 0; k < suf; k++ {
		ia, ib := len(a)-suf+k, len(b)-suf+k
		ops = append(ops, diffOp{' ', a[ia], ia, ib})
	}
	return ops
}
