// SPDX-License-Identifier: GPL-3.0-or-later

package devrun

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/harness"
)

func writeNodeJSON(t *testing.T, codex map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(map[string]any{"host": "devbox", "codex": codex})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dev run gives codex node.json's codex settings, as the node does its
// sessions, and says so.
func TestNodeCodexOptions(t *testing.T) {
	home := t.TempDir()
	dir := writeNodeJSON(t, map[string]any{
		"bin":         "/opt/codex",
		"home":        home,
		"args":        []string{"app-server", "-c", "model_provider=\"deepseek\""},
		"config":      map[string]any{"sandbox_workspace_write": map[string]any{"network_access": true}},
		"permissions": "net",
	})
	var errb bytes.Buffer
	var opts harness.Options
	if err := nodeCodexOptions(dir, &opts, &errb); err != nil {
		t.Fatal(err)
	}
	if opts.CodexBin != "/opt/codex" || opts.CodexPermissions != "net" || opts.CodexEnv["CODEX_HOME"] != home ||
		strings.Join(opts.CodexArgs, " ") != `app-server -c model_provider="deepseek"` ||
		opts.CodexConfig["sandbox_workspace_write.network_access"] != "true" {
		t.Fatalf("options: %+v", opts)
	}
	if !strings.Contains(errb.String(), "codex settings from "+filepath.Join(dir, "node.json")) {
		t.Fatalf("not said: %q", errb.String())
	}

	// No node.json: said, not an error.
	errb.Reset()
	opts = harness.Options{}
	if err := nodeCodexOptions(t.TempDir(), &opts, &errb); err != nil || opts.CodexArgs != nil || opts.CodexConfig != nil ||
		!strings.Contains(errb.String(), "codex runs with its own settings") {
		t.Fatalf("no node.json: %v %+v %q", err, opts, errb.String())
	}

	// A node.json the node would refuse is refused here too.
	bad := writeNodeJSON(t, map[string]any{"args": []string{"-c", "x=1"}})
	if err := nodeCodexOptions(bad, &opts, &errb); err == nil || !strings.Contains(err.Error(), "no subcommand") {
		t.Fatalf("bad node.json: %v", err)
	}
}

// Main uses node.json's codex.bin without -codex-bin.
func TestMainUsesNodeConfig(t *testing.T) {
	t.Setenv("SWK_FAKE_APPSERVER", "1")
	t.Setenv("SWK_FAKE_STATE", t.TempDir())
	dir := writeNodeJSON(t, map[string]any{"bin": os.Args[0]})
	var out, errb bytes.Buffer
	if code := Main([]string{"-node-dir", dir, "-cwd", t.TempDir(), "hello"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "codex settings from") || !strings.Contains(out.String(), "hello") {
		t.Fatalf("out %q err %q", out.String(), errb.String())
	}
}
