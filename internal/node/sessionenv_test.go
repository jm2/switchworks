// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSessionEnvSet(t *testing.T) {
	// The Windows git example: plain variables pass through as given.
	e := SessionEnv{Set: map[string]string{
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.sslBackend", "GIT_CONFIG_VALUE_0": "openssl",
	}}
	got, err := sessionEnv(e, []string{"PATH=/usr/bin", "HOME=/home/u"}, false, ":")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.sslBackend", "GIT_CONFIG_VALUE_0": "openssl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Nothing set, nothing changed: not even PATH.
	if got, err := sessionEnv(SessionEnv{}, []string{"PATH=/usr/bin"}, false, ":"); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestSessionEnvPath(t *testing.T) {
	abs := func(p string) string {
		if runtime.GOOS == "windows" {
			return `C:` + filepath.FromSlash(p)
		}
		return p
	}
	tools, extra := abs("/opt/tools/glslang"), abs("/opt/late")
	if runtime.GOOS != "windows" { // ':' is in a Windows path
		e := SessionEnv{PathPrepend: []string{tools}, PathAppend: []string{extra}}
		environ := []string{"HOME=/home/u", "PATH=/usr/bin" + ":" + tools + ":/bin"}
		got, err := sessionEnv(e, environ, false, ":")
		if err != nil {
			t.Fatal(err)
		}
		// The node's PATH, with the directories added once each.
		if want := tools + ":/usr/bin:/bin:" + extra; got["PATH"] != want {
			t.Fatalf("PATH %q, want %q", got["PATH"], want)
		}
		if len(got) != 1 {
			t.Fatalf("only PATH should be set: %v", got)
		}
	}

	// Windows: the node's own spelling ("Path"), ';' and case-folded
	// duplicates. A set.PATH replaces the base.
	win := SessionEnv{PathPrepend: []string{tools}}
	got, err := sessionEnv(win, []string{`Path=C:\Windows;` + strings.ToUpper(tools)}, true, ";")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Path": tools + `;C:\Windows`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows: got %v, want %v", got, want)
	}
	win.Set = map[string]string{"path": `C:\Only`, "temp": `C:\T`}
	got, err = sessionEnv(win, []string{`Path=C:\Windows`, `TEMP=C:\old`}, true, ";")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Path": tools + `;C:\Only`, "TEMP": `C:\T`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows set: got %v, want %v", got, want)
	}

	// No PATH in the node's environment at all.
	got, err = sessionEnv(SessionEnv{PathAppend: []string{tools}}, nil, false, string(os.PathListSeparator))
	if err != nil || got["PATH"] != tools {
		t.Fatalf("no PATH: %v %v", got, err)
	}
}

func TestSessionEnvRefuses(t *testing.T) {
	for name, e := range map[string]SessionEnv{
		"relative dir":  {PathPrepend: []string{"bin"}},
		"dot":           {PathAppend: []string{"."}},
		"list in entry": {PathPrepend: []string{"/a:/b"}},
		"bad name":      {Set: map[string]string{"A=B": "x"}},
		"empty name":    {Set: map[string]string{"": "x"}},
		"NUL value":     {Set: map[string]string{"A": "x\x00y"}},
		"CODEX_HOME":    {Set: map[string]string{"CODEX_HOME": "/x"}},
	} {
		if runtime.GOOS == "windows" && name == "list in entry" {
			continue
		}
		if _, err := sessionEnv(e, []string{"PATH=/usr/bin"}, false, ":"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCodexEnvFromConfig(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	dir := t.TempDir()
	c := nodeConfig(t, `{"codex": {"home": "/h", "session_env": {"set": {"A": "1"}, "path_prepend": [`+jsonString(dir)+`]}}}`)
	env, err := codexEnv(c)
	if err != nil {
		t.Fatal(err)
	}
	pathKey := "PATH"
	if runtime.GOOS == "windows" {
		pathKey = "Path"
		for k := range env {
			if strings.EqualFold(k, "PATH") {
				pathKey = k
			}
		}
	}
	if env["CODEX_HOME"] != "/h" || env["A"] != "1" || !strings.HasPrefix(env[pathKey], dir+string(filepath.ListSeparator)) {
		t.Fatalf("env %v", env)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
