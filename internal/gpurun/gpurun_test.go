// SPDX-License-Identifier: GPL-3.0-or-later

package gpurun

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestValidate(t *testing.T) {
	ok := Request{Argv: []string{"game.exe", "--test"}, Env: map[string]string{"SDL_VIDEODRIVER": "windows", "ProgramFiles(x86)": "x"}, TimeoutSeconds: 60}
	if err := Validate(ok, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Request{
		"no argv":     {},
		"blank argv0": {Argv: []string{" "}},
		"nul arg":     {Argv: []string{"a", "b\x00c"}},
		"long":        {Argv: []string{"a", strings.Repeat("x", maxArgBytes)}},
		"env key":     {Argv: []string{"a"}, Env: map[string]string{"A=B": "c"}},
		"env empty":   {Argv: []string{"a"}, Env: map[string]string{"": "c"}},
		"env nul":     {Argv: []string{"a"}, Env: map[string]string{"A": "c\x00"}},
		"negative":    {Argv: []string{"a"}, TimeoutSeconds: -1},
		"too long":    {Argv: []string{"a"}, TimeoutSeconds: 601},
		"cwd nul":     {Argv: []string{"a"}, Cwd: "a\x00"},
	}
	for name, r := range bad {
		if err := Validate(r, 10*time.Minute); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (Request{}).Timeout() != DefaultTimeout || (Request{TimeoutSeconds: 7}).Timeout() != 7*time.Second {
		t.Error("Timeout")
	}
}

func TestResolveCwd(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	out, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(ws, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := os.Symlink(out, filepath.Join(ws, "escape")) == nil // Windows: needs developer mode
	os.WriteFile(filepath.Join(ws, "file"), nil, 0o644)
	for in, want := range map[string]string{"": ws, ".": ws, "sub/dir": filepath.Join(ws, "sub", "dir"), filepath.Join(ws, "sub"): filepath.Join(ws, "sub")} {
		got, err := ResolveCwd(ws, in)
		if err != nil || got != want {
			t.Errorf("ResolveCwd(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	refused := []string{"..", out, "missing", "file"}
	if links {
		refused = append(refused, "escape")
	}
	for _, in := range refused {
		if got, err := ResolveCwd(ws, in); err == nil {
			t.Errorf("ResolveCwd(%q) = %q, accepted", in, got)
		}
	}
}

// abs makes a test path absolute on this OS.
func abs(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}
	return p
}

func TestCheckWorkspace(t *testing.T) {
	home := abs("/home/u")
	deny := []string{abs("/home/u/.ssh"), abs("/home/u/secret")}
	sys := []string{abs("/Windows"), abs("/Program Files")}
	if err := CheckWorkspace(abs("/home/u/src/game"), home, deny, sys); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && CheckWorkspace(`c:\HOME\u`, home, nil, nil) == nil {
		t.Error("case")
	}
	for _, ws := range []string{"/home/u", "/home", "/", "/home/u/secret", "/home/u/secret/x", "/Windows", "/Windows/Temp", "/Program Files/Game"} {
		if ws = abs(ws); CheckWorkspace(ws, home, deny, sys) == nil {
			t.Errorf("%s accepted", ws)
		}
	}
}

func TestProtected(t *testing.T) {
	ws := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(ws, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(".git/hooks")
	mk("a/b/c/.git")    // depth 4: found
	mk("a/b/c/d/.git")  // depth 5: not
	mk("vendor/x/.git") // nested repos too
	mk("keys/inner/.git")
	os.WriteFile(filepath.Join(ws, "sub.git"), nil, 0o644)
	mk("wt")
	os.WriteFile(filepath.Join(ws, "wt", ".git"), []byte("gitdir: x"), 0o644) // a worktree's file
	os.WriteFile(filepath.Join(ws, ".env"), nil, 0o600)
	deny := []string{filepath.Join(ws, "keys"), filepath.Join(ws, ".env"), filepath.Join(ws, "missing"), "/elsewhere"}
	ro, hidden := Protected(ws, deny)
	wantRO := []string{filepath.Join(ws, ".git"), filepath.Join(ws, "a/b/c/.git"), filepath.Join(ws, "vendor/x/.git"), filepath.Join(ws, "wt/.git")}
	wantHidden := []string{filepath.Join(ws, ".env"), filepath.Join(ws, "keys")}
	if !reflect.DeepEqual(ro, wantRO) {
		t.Errorf("read-only %q, want %q", ro, wantRO)
	}
	if !reflect.DeepEqual(hidden, wantHidden) {
		t.Errorf("hidden %q, want %q", hidden, wantHidden)
	}
}

func TestProgramDir(t *testing.T) {
	env := map[string]string{"SystemRoot": abs("/Windows"), "ProgramFiles": abs("/Program Files")}
	sys := SystemDirs(func(k string) string { return env[k] }, abs("/home/u"))
	if len(sys) != 3 || sys[2] != filepath.Join(abs("/home/u"), "AppData", "Local", "Packages") {
		t.Fatalf("%q", sys)
	}
	ws := abs("/home/u/src/game")
	for _, p := range []string{"/home/u/src/game/build/g.exe", "/Windows/System32/cmd.exe", "/Program Files/Tool/t.exe"} {
		if err := CheckProgramDir(abs(p), ws, sys); err != nil {
			t.Error(err)
		}
	}
	for _, p := range []string{"/home/u/AppData/Local/Programs/Python/python.exe", "/home/u/.ssh/x.exe", "/opt/g"} {
		if CheckProgramDir(abs(p), ws, sys) == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestContainerName(t *testing.T) {
	a, b := ContainerName("/w/one"), ContainerName("/w/two")
	if a == b || len(a) > 64 || !strings.HasPrefix(a, "swk.gpu.") || a != ContainerName("/w/one/") {
		t.Errorf("%q %q", a, b)
	}
}

func TestEnvironment(t *testing.T) {
	node := map[string]string{"SystemRoot": `C:\Windows`, "ProgramFiles": `C:\Program Files`, "PATH": `C:\Users\u\bin`,
		"GH_TOKEN": "secret", "USERPROFILE": `C:\Users\u`, "APPDATA": `C:\Users\u\AppData\Roaming`}
	env := Environment(func(k string) string { return node[k] }, `C:\ac`, map[string]string{"sdl_videodriver": "windows", "Path": `C:\tools`})
	got := strings.Join(env, "\n")
	for _, s := range []string{"GH_TOKEN", `C:\Users\u`} {
		if strings.Contains(got, s) {
			t.Errorf("environment leaks %q:\n%s", s, got)
		}
	}
	for _, s := range []string{`SystemRoot=C:\Windows`, `ProgramFiles=C:\Program Files`, "TEMP=" + filepath.Join(`C:\ac`, "Temp"),
		"APPDATA=" + filepath.Join(`C:\ac`, "Roaming"), "sdl_videodriver=windows", `Path=C:\tools`} {
		if !strings.Contains(got, s) {
			t.Errorf("environment lacks %q:\n%s", s, got)
		}
	}
	if strings.Count(strings.ToUpper(got), "\nPATH=") != 1 {
		t.Errorf("PATH twice:\n%s", got)
	}
	block := EnvBlock([]string{"A=1", "B=é"})
	if want := append(utf16.Encode([]rune("A=1\x00B=é\x00")), 0); !reflect.DeepEqual(block, want) {
		t.Errorf("EnvBlock = %v", block)
	}
	if !reflect.DeepEqual(EnvBlock(nil), []uint16{0, 0}) {
		t.Error("empty EnvBlock")
	}
}

func TestCommandLine(t *testing.T) {
	cases := map[string][]string{
		`game.exe --test`:             {"game.exe", "--test"},
		`"C:\Program Files\g.exe" ""`: {`C:\Program Files\g.exe`, ""},
		`a "b \"c\"" d\ "e f\\"`:      {"a", `b "c"`, `d\`, `e f\`},
		`a b\c "x\\\"y"`:              {"a", `b\c`, `x\"y`},
		`a "tab	x"`:                   {"a", "tab\tx"},
	}
	for want, argv := range cases {
		if got := CommandLine(argv); got != want {
			t.Errorf("CommandLine(%q) = %s, want %s", argv, got, want)
		}
	}
	for _, p := range []string{`C:\x\run.bat`, `y.CMD`} {
		if CheckProgram(p) == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if CheckProgram(`C:\x\game.exe`) != nil {
		t.Error("exe refused")
	}
}

func TestTail(t *testing.T) {
	tl := NewTail(8)
	tl.Write([]byte("hello "))
	tl.Write([]byte("world"))
	if tl.String() != "lo world" || tl.Dropped() != 3 {
		t.Errorf("%q %d", tl.String(), tl.Dropped())
	}
	tl.Write([]byte("0123456789"))
	if tl.String() != "23456789" || tl.Dropped() != 13 {
		t.Errorf("%q %d", tl.String(), tl.Dropped())
	}
	u := NewTail(3)
	u.Write([]byte("aé")) // 3 bytes; then one more cuts é
	u.Write([]byte("b"))
	if !strings.HasSuffix(u.String(), "b") || strings.ContainsRune(u.String(), 0xC3) {
		t.Errorf("%q", u.String())
	}
}

func TestFindProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and exec bits; see run_windows_test.go")
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(ws, "bin"), 0o755)
	os.WriteFile(filepath.Join(ws, "bin", "game"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(ws, "notexec"), nil, 0o644)
	os.WriteFile(filepath.Join(ws, "game2"), []byte("#!/bin/sh\n"), 0o755)
	if p, err := FindProgram("bin/game", ws); err != nil || p != filepath.Join(ws, "bin", "game") {
		t.Errorf("relative: %q %v", p, err)
	}
	if p, err := FindProgram("sh", ws); err != nil || !filepath.IsAbs(p) {
		t.Errorf("PATH: %q %v", p, err)
	}
	t.Chdir(ws)
	t.Setenv("PATH", ".")
	for _, a := range []string{"game2", "notexec", "missing/x"} {
		if p, err := FindProgram(a, ws); err == nil {
			t.Errorf("%s found: %s", a, p)
		}
	}
}
