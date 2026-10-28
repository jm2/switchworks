// SPDX-License-Identifier: GPL-3.0-or-later

package codex

import (
	"errors"
	"strconv"
	"strings"
)

// The node reads a few settings from config.toml without a TOML parser
// (Switchworks has no dependencies): the plain forms, a key in its table
// or dotted in full, with single-line or multi-line arrays of strings.
// Anything it cannot read is reported as unreadable, never guessed at.

// errUnterminated: the array goes on past the text given.
var errUnterminated = errors.New("unterminated array")

// ParseStringArray reads a TOML array of strings ("basic" or 'literal',
// with comments, newlines and a trailing comma) from the start of s.
func ParseStringArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return nil, errors.New("not an array")
	}
	s = s[1:]
	var out []string
	needComma := false
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case s == "":
			return nil, errUnterminated
		case s[0] == '#':
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return nil, errUnterminated
			}
			s = s[i+1:]
		case s[0] == ']':
			return out, nil
		case s[0] == ',':
			if !needComma {
				return nil, errors.New("misplaced comma")
			}
			needComma = false
			s = s[1:]
		case needComma:
			return nil, errors.New("missing comma")
		case strings.HasPrefix(s, `"""`), strings.HasPrefix(s, `'''`):
			return nil, errors.New("multi-line strings are not read")
		case s[0] == '"':
			end := 1
			for end < len(s) && s[end] != '"' && s[end] != '\n' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) || s[end] != '"' {
				return nil, errors.New("unterminated string")
			}
			v, err := strconv.Unquote(s[:end+1])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			s, needComma = s[end+1:], true
		case s[0] == '\'':
			end := strings.IndexAny(s[1:], "'\n")
			if end < 0 || s[1+end] != '\'' {
				return nil, errors.New("unterminated string")
			}
			out = append(out, s[1:end+1])
			s, needComma = s[end+2:], true
		default:
			return nil, errors.New("not an array of strings")
		}
	}
}

// assignment is what scanConfig found for a key.
type assignment struct {
	found bool   // the key, or a table holding it, is set
	value string // the key's value as written, when set as itself
}

// scanConfig looks for the key path (match decides each part) in
// config.toml. A key set as an inline table holding it (for example
// sandbox_workspace_write = {writable_roots = [...]}) counts as found,
// with no value. An array value spanning lines is returned whole.
func scanConfig(toml []byte, path []string, match func(i int, part string) bool) assignment {
	is := func(full []string, n int) bool {
		if len(full) != n {
			return false
		}
		for i := range n {
			if !match(i, full[i]) {
				return false
			}
		}
		return true
	}
	lines := strings.Split(string(toml), "\n")
	var table []string
	inMulti := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if inMulti != "" {
			if strings.Count(line, inMulti)%2 == 1 {
				inMulti = ""
			}
			continue
		}
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			keys, rest, ok := splitKey(strings.TrimPrefix(line[1:], "["))
			table = nil
			if ok && strings.HasPrefix(strings.TrimSpace(rest), "]") {
				table = keys
			}
			continue
		}
		for _, q := range []string{`"""`, `'''`} {
			if strings.Count(line, q)%2 == 1 {
				inMulti = q
			}
		}
		keys, rest, ok := splitKey(line)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		full := append(append([]string{}, table...), keys...)
		for n := 1; n < len(path); n++ {
			if is(full, n) {
				return assignment{found: true} // an inline table, or a value that will not load
			}
		}
		if !is(full, len(path)) {
			continue
		}
		v := strings.TrimSpace(rest[1:])
		if strings.HasPrefix(v, "[") {
			for j := i + 1; j < len(lines) && j < i+10000; j++ {
				if _, err := ParseStringArray(v); !errors.Is(err, errUnterminated) {
					break
				}
				v += "\n" + lines[j]
			}
		}
		return assignment{found: true, value: v}
	}
	return assignment{}
}

func exact(path []string) func(int, string) bool {
	return func(i int, part string) bool { return path[i] == part }
}

// ConfigWritableRoots reads sandbox_workspace_write.writable_roots from
// config.toml. set reports whether it is there; ok is false when it is
// there in a form not read here.
func ConfigWritableRoots(toml []byte) (roots []string, set, ok bool) {
	path := []string{"sandbox_workspace_write", "writable_roots"}
	a := scanConfig(toml, path, exact(path))
	if !a.found {
		return nil, false, true
	}
	if a.value == "" {
		return nil, true, false
	}
	roots, err := ParseStringArray(a.value)
	return roots, true, err == nil
}

// ConfigSetsEnv reports whether config.toml sets name (in any case) in
// shell_environment_policy.set, or sets that table in a form not read
// here.
func ConfigSetsEnv(toml []byte, name string) bool {
	path := []string{"shell_environment_policy", "set", name}
	return scanConfig(toml, path, func(i int, part string) bool {
		if i == 2 {
			return strings.EqualFold(part, name)
		}
		return path[i] == part
	}).found
}
