// SPDX-License-Identifier: GPL-3.0-or-later

package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Config overrides are what `codex -c key=value` takes: a dotted key path
// and a TOML value. Codex splits the key on dots without unquoting, so
// each segment must be a bare TOML key.

// FlattenConfig turns a JSON object of config overrides (node.json
// codex.config) into dotted keys and TOML-encoded values. Nested objects
// become dotted keys: {"features": {"apps": false}} is features.apps=false.
func FlattenConfig(m map[string]json.RawMessage) (map[string]string, error) {
	out := map[string]string{}
	for k, raw := range m {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // keep 128000 as written, not 1.28e+05
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("codex.config %q: %w", k, err)
		}
		if err := flatten(out, k, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func flatten(out map[string]string, key string, v any) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for k, sub := range m {
			if err := flatten(out, key+"."+k, sub); err != nil {
				return err
			}
		}
		return nil
	}
	s, err := EncodeTOML(v)
	if err != nil {
		return fmt.Errorf("codex.config %q: %w", key, err)
	}
	if _, dup := out[key]; dup {
		return fmt.Errorf("codex.config sets %q twice", key)
	}
	out[key] = s
	return nil
}

// checkKey accepts a dotted path of bare keys (letters, digits, _ and -).
func checkKey(key string) error {
	for _, seg := range strings.Split(key, ".") {
		if seg == "" {
			return fmt.Errorf("codex.config key %q: empty segment", key)
		}
		for _, r := range seg {
			if !bareKeyChar(r) {
				return fmt.Errorf("codex.config key %q: only letters, digits, _ and - may appear between the dots", key)
			}
		}
	}
	return nil
}

func bareKeyChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

// EncodeTOML renders a decoded JSON value (decoded with UseNumber) as a
// TOML value: strings quoted and escaped, numbers as written, booleans,
// arrays, and objects as inline tables. null has no TOML form.
func EncodeTOML(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return tomlString(x), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		// JSON numbers are valid TOML integers or floats as written.
		return x.String(), nil
	case float64:
		b, _ := json.Marshal(x)
		return string(b), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, err := EncodeTOML(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := EncodeTOML(x[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, tomlKey(k)+" = "+s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case nil:
		return "", errors.New("null has no TOML value; leave the key out instead")
	}
	return "", fmt.Errorf("unsupported value %T", v)
}

// tomlKey quotes a key inside an inline table unless it is bare.
func tomlKey(k string) string {
	if k == "" {
		return `""`
	}
	for _, r := range k {
		if !bareKeyChar(r) {
			return tomlString(k)
		}
	}
	return k
}

// tomlString renders s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// OverrideArgs renders overrides as -c arguments, sorted by key.
func OverrideArgs(overrides map[string]string) []string {
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, "-c", k+"="+overrides[k])
	}
	return out
}

// valueFlags are codex options that take the next argument as their value.
var valueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true,
	"-m": true, "--model": true, "-p": true, "--profile": true,
	"-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true,
	"-C": true, "--cd": true, "--add-dir": true, "-i": true, "--image": true,
	"--local-provider": true,
}

// Subcommand returns the first argument of a codex argv that is neither an
// option nor an option's value ("app-server"), or "" if there is none.
func Subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "-") {
			if valueFlags[a] {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// ConfigKeys returns the keys a codex argv overrides with -c or --config
// (the forms "-c k=v", "-ck=v", "--config k=v" and "--config=k=v"), before
// or after its subcommand.
func ConfigKeys(args []string) []string {
	var keys []string
	add := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return keys
		case a == "-c" || a == "--config":
			if i+1 < len(args) {
				add(args[i+1])
			}
			i++
		case strings.HasPrefix(a, "--config="):
			add(strings.TrimPrefix(a, "--config="))
		case strings.HasPrefix(a, "-c") && !strings.HasPrefix(a, "--"):
			add(strings.TrimPrefix(a, "-c"))
		case valueFlags[a]:
			i++
		}
	}
	return keys
}

// ConfigSetsWindowsSandbox reports whether a config.toml sets a Windows
// sandbox mode: sandbox in a [windows] table, or windows.sandbox at the top
// level (as a dotted key or in an inline windows table). It looks only at
// table headers and key names, never at values (config.toml holds provider
// keys), and does not follow profiles.
func ConfigSetsWindowsSandbox(toml []byte) bool {
	table := ""
	for _, line := range strings.Split(string(toml), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			name, _, _ := strings.Cut(strings.Trim(line, "[ \t"), "]")
			table = normalizeKey(name)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = normalizeKey(key)
		switch {
		case table == "windows" && key == "sandbox",
			table == "" && key == "windows.sandbox",
			table == "" && key == "windows" && strings.Contains(value, "sandbox"):
			return true
		}
	}
	return false
}

// normalizeKey drops quotes and blanks around the parts of a TOML key.
func normalizeKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}
