// SPDX-License-Identifier: GPL-3.0-or-later

// Package gpurun runs a command for a sandboxed agent where it can use the
// GPU, confined (Windows: see run_windows.go).
//
// Codex's Windows sandbox runs commands on a private desktop
// (CodexSandboxDesktop-*), where Direct3D 9 and 11 cannot reach the
// display: device creation fails (D3DERR_NOTAVAILABLE). The node runs in the
// owner's interactive session, so it can start a command on the interactive
// desktop (winsta0\default). It does so inside an AppContainer, in a job
// object: the command sees the workspace (read-write, every .git read-only),
// what Windows grants all AppContainers (system files, Program Files), and
// nothing of the owner's profile. It has no network unless the node allows
// it, a time limit, and job UI limits (no clipboard, no other processes'
// windows, no desktop switching, no logoff). Output is captured and returned.
//
// This file holds what does not depend on the OS, so it is tested anywhere.
package gpurun

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// ErrUnsupported is returned where confined GPU runs are not implemented.
var ErrUnsupported = errors.New("GPU runs through the node are only implemented on Windows; on Linux use swk-gpu-run")

// DefaultDesktop is the interactive desktop.
const DefaultDesktop = `winsta0\default`

// Limits.
const (
	DefaultTimeout = 5 * time.Minute
	MaxOutput      = 64 << 10 // bytes kept per stream (the end of it)
	maxArgs        = 256
	maxArgBytes    = 32 << 10 // a Windows command line is at most 32767 UTF-16 units
	maxEnv         = 128
	maxEnvBytes    = 32 << 10
)

// Request is an agent's GPU run, as the node's local API takes it.
type Request struct {
	// Argv is the command and its arguments. Argv[0] is a program name,
	// looked up on the node's PATH, or a path (relative to Cwd).
	Argv []string `json:"argv"`
	// Cwd is the working directory, relative to the session's working
	// directory (the workspace) or absolute inside it. Default: the
	// workspace.
	Cwd string `json:"cwd,omitempty"`
	// Env adds environment variables. The command does not get the node's
	// environment, only a fixed base (system paths, TEMP and APPDATA in its
	// own folder) and these.
	Env map[string]string `json:"env,omitempty"`
	// TimeoutSeconds is the time limit (default DefaultTimeout; at most the
	// node's limit). The command and everything it started is killed then.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Result is a finished run.
type Result struct {
	ExitCode   int64  `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	// StdoutDropped and StderrDropped count bytes dropped from the start
	// of each stream (only the last MaxOutput bytes are kept).
	StdoutDropped int64 `json:"stdout_dropped,omitempty"`
	StderrDropped int64 `json:"stderr_dropped,omitempty"`
	// Skipped counts workspace entries the container was not given access
	// to: links (symlinks, junctions, hard links), entries whose path
	// changed while they were opened, and entries whose ACL the node
	// cannot change.
	Skipped int `json:"skipped,omitempty"`
}

// Plan is a checked run, ready for Run.
type Plan struct {
	Workspace string // the session's working directory, symlinks resolved
	Cwd       string // inside Workspace
	Program   string // absolute path of the executable
	Argv      []string
	Env       map[string]string // added to the base environment
	Timeout   time.Duration
	Network   bool // give the AppContainer the client network capabilities
	// Desktop is the desktop the command runs on (Windows: DefaultDesktop,
	// where Direct3D reaches the display); "" inherits the node's.
	Desktop string
	// ReadOnly are paths in Workspace the command may read but not change,
	// delete or rename: every .git. Hidden are paths it may not open at all:
	// the deny list's entries inside the workspace.
	ReadOnly, Hidden []string
}

// Validate checks a request's fields that need no file system.
func Validate(r Request, maxTimeout time.Duration) error {
	if len(r.Argv) == 0 || strings.TrimSpace(r.Argv[0]) == "" {
		return errors.New("argv: give the command and its arguments, e.g. [\"game.exe\", \"--test\"]")
	}
	if len(r.Argv) > maxArgs {
		return fmt.Errorf("argv: at most %d arguments", maxArgs)
	}
	n := 0
	for _, a := range r.Argv {
		if strings.ContainsRune(a, 0) {
			return errors.New("argv: arguments cannot contain NUL")
		}
		n += len(a) + 3
	}
	if n > maxArgBytes {
		return fmt.Errorf("argv: the command line is over %d bytes", maxArgBytes)
	}
	if len(r.Env) > maxEnv {
		return fmt.Errorf("env: at most %d variables", maxEnv)
	}
	n = 0
	for k, v := range r.Env {
		if !validEnvKey(k) {
			return fmt.Errorf("env: bad variable name %q", k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("env: %s contains NUL", k)
		}
		n += len(k) + len(v) + 2
	}
	if n > maxEnvBytes {
		return fmt.Errorf("env: over %d bytes", maxEnvBytes)
	}
	if strings.ContainsRune(r.Cwd, 0) {
		return errors.New("cwd: contains NUL")
	}
	if r.TimeoutSeconds < 0 {
		return errors.New("timeout_seconds: must be positive")
	}
	if d := time.Duration(r.TimeoutSeconds) * time.Second; d > maxTimeout {
		return fmt.Errorf("timeout_seconds: at most %d on this node", int(maxTimeout/time.Second))
	}
	return nil
}

func validEnvKey(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for _, c := range k {
		if !(c == '_' || c == '(' || c == ')' || c == '.' || c == '-' ||
			'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return false
		}
	}
	return true
}

// Timeout is the request's time limit.
func (r Request) Timeout() time.Duration {
	if r.TimeoutSeconds == 0 {
		return DefaultTimeout
	}
	return time.Duration(r.TimeoutSeconds) * time.Second
}

// ResolveCwd returns the run's working directory: cwd (relative to ws, or
// absolute) with symlinks resolved, which must be ws or below it. ws must
// already be resolved.
func ResolveCwd(ws, cwd string) (string, error) {
	d := cwd
	if d == "" {
		d = ws
	} else if !filepath.IsAbs(d) {
		d = filepath.Join(ws, d)
	}
	real, err := filepath.EvalSymlinks(d)
	if err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	if !Within(ws, real) {
		return "", fmt.Errorf("cwd %q is outside the session's working directory %s", cwd, ws)
	}
	return real, nil
}

// Within reports whether p is root or below it (both clean and absolute;
// case-insensitive on Windows).
func Within(root, p string) bool {
	rel, err := filepath.Rel(fold(root), fold(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func fold(p string) string {
	p = filepath.Clean(p)
	if filepath.Separator == '\\' {
		return strings.ToLower(p)
	}
	return p
}

// CheckWorkspace refuses a workspace the command must not be given: the
// home directory or one above it (it holds the owner's credentials), or one
// inside a denied path.
func CheckWorkspace(ws, home string, deny []string) error {
	if home != "" && Within(ws, home) {
		return fmt.Errorf("the session works in %s, which holds your home directory; GPU runs need a project directory", ws)
	}
	for _, d := range deny {
		if d != "" && Within(d, ws) {
			return fmt.Errorf("the session's working directory %s is inside %s, which the deny list covers", ws, d)
		}
	}
	return nil
}

// gitDepth is how deep under the workspace .git entries are looked for,
// as swk-gpu-sandbox does on Linux.
const gitDepth = 4

// Protected returns the paths in ws the command must not change: every .git
// (directory or file) up to gitDepth levels down, and the deny list's
// existing entries inside ws, which it must not open. Directories in deny
// are not descended into.
func Protected(ws string, deny []string) (readOnly, hidden []string) {
	for _, d := range deny {
		if d == "" || !Within(ws, d) {
			continue
		}
		if _, err := os.Lstat(d); err == nil {
			hidden = append(hidden, filepath.Clean(d))
		}
	}
	isHidden := func(p string) bool {
		for _, h := range hidden {
			if Within(h, p) {
				return true
			}
		}
		return false
	}
	base := strings.Count(filepath.Clean(ws), string(filepath.Separator))
	_ = filepath.WalkDir(ws, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if e != nil && e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != ws && isHidden(p) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(e.Name(), ".git") && p != ws {
			readOnly = append(readOnly, p)
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// WalkDir does not follow symlinks or (with winsymlink=0) junctions.
		if e.IsDir() && strings.Count(p, string(filepath.Separator))-base >= gitDepth {
			return fs.SkipDir
		}
		return nil
	})
	sort.Strings(readOnly)
	sort.Strings(hidden)
	return readOnly, hidden
}

// ContainerName is the AppContainer name for a workspace: one per
// workspace, so a run in one cannot write another's files. At most 64
// characters, as Windows requires.
func ContainerName(ws string) string {
	sum := sha256.Sum256([]byte(fold(ws)))
	return "swk.gpu." + hex.EncodeToString(sum[:10])
}

// baseEnvKeys are the node's variables a run keeps: where Windows and
// programs are, and what the processor is. Not the profile's folders, nor
// anything a tool may have put there (tokens, proxies).
var baseEnvKeys = []string{
	"SystemRoot", "windir", "SystemDrive", "ComSpec", "PATHEXT", "OS",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
	"PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL", "PROCESSOR_REVISION",
	"NUMBER_OF_PROCESSORS", "DriverData", "PUBLIC",
}

// Environment builds the run's environment block entries: the base from
// getenv, the container folder's paths (folder is the AppContainer's own
// folder, which it may write), then extra. Keys are matched without regard
// to case, as Windows does; the result is sorted.
func Environment(getenv func(string) string, folder string, extra map[string]string) []string {
	env := map[string]string{} // folded key -> "Key=value"
	set := func(k, v string) { env[strings.ToUpper(k)] = k + "=" + v }
	for _, k := range baseEnvKeys {
		if v := getenv(k); v != "" {
			set(k, v)
		}
	}
	if sr := getenv("SystemRoot"); sr != "" {
		sys := filepath.Join(sr, "System32")
		set("PATH", strings.Join([]string{sys, sr, filepath.Join(sys, "Wbem"), filepath.Join(sys, "WindowsPowerShell", "v1.0")}, ";"))
	}
	if folder != "" {
		tmp := filepath.Join(folder, "Temp")
		set("TEMP", tmp)
		set("TMP", tmp)
		set("USERPROFILE", folder)
		set("HOME", folder)
		set("APPDATA", filepath.Join(folder, "Roaming"))
		set("LOCALAPPDATA", filepath.Join(folder, "Local"))
	}
	for k, v := range extra {
		set(k, v)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = env[k]
	}
	return out
}

// FolderDirs are the directories Environment points into, created in the
// container folder before a run.
var FolderDirs = []string{"Temp", "Roaming", "Local"}

// EnvBlock encodes env as a CREATE_UNICODE_ENVIRONMENT block.
func EnvBlock(env []string) []uint16 {
	var b []uint16
	for _, kv := range env {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}

// CommandLine joins argv for CreateProcess, quoting as the C runtime's
// argument parser expects (the same rules as Go's syscall.EscapeArg).
func CommandLine(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(escapeArg(a))
	}
	return b.String()
}

func escapeArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	for ; slashes > 0; slashes-- {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// FindProgram resolves argv0 for a run in cwd: a path (one with a
// separator) is taken relative to cwd; a bare name is looked up on the
// node's PATH, never in the current directory. On Windows both try PATHEXT.
func FindProgram(argv0, cwd string) (string, error) {
	p := argv0
	if strings.ContainsAny(argv0, `/\`) || filepath.VolumeName(argv0) != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
	}
	found, err := exec.LookPath(p)
	if err != nil || !filepath.IsAbs(found) {
		return "", fmt.Errorf("%s: no such program (a bare name is looked up on the node's PATH; give a path for one in the workspace)", argv0)
	}
	if err := CheckProgram(found); err != nil {
		return "", err
	}
	return found, nil
}

// CheckProgram refuses programs CreateProcess would hand to cmd.exe, whose
// own parsing the quoting above does not cover.
func CheckProgram(path string) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bat", ".cmd":
		return fmt.Errorf("%s is a batch file; run it as [\"cmd.exe\", \"/d\", \"/c\", ...]", filepath.Base(path))
	}
	return nil
}

// Tail keeps the last max bytes written to it and counts the rest. It is
// safe for concurrent use.
type Tail struct {
	mu      sync.Mutex
	max     int
	buf     []byte
	dropped int64
}

// NewTail returns a Tail keeping max bytes.
func NewTail(max int) *Tail { return &Tail{max: max} }

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= t.max {
		t.dropped += int64(len(t.buf) + len(p) - t.max)
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns what is kept, as valid UTF-8 (a character cut at the
// start is dropped).
func (t *Tail) String() string { return strings.ToValidUTF8(string(t.buf), "�") }

// Dropped is the number of bytes not kept.
func (t *Tail) Dropped() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}
