// SPDX-License-Identifier: GPL-3.0-or-later

// Package codexad adapts `codex app-server` to the adapter interface.
//
// One app-server process per node hosts every managed thread; notifications
// are routed to sessions by threadId. If app-server exits, the adapter starts
// a new one and resumes every live thread with thread/resume.
package codexad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/codex"
	"github.com/jm2/switchworks/internal/jsonrpc"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/proc"
	"github.com/jm2/switchworks/internal/ulid"
)

// Harness is this adapter's name.
const Harness = "codex"

// Config configures the adapter.
type Config struct {
	Bin    string            // codex binary; default "codex"
	Args   []string          // default ["app-server"]
	Env    map[string]string // e.g. CODEX_HOME
	Dir    string            // app-server working directory
	Stderr io.Writer         // app-server logs
	Tap    jsonrpc.Tap       // raw protocol capture
	Logf   func(format string, args ...any)

	// Permissions names a permission profile in codex's config.toml
	// ([permissions.<id>]) for workspace-write sessions to run under in
	// place of codex's legacy workspace-write sandbox. Codex ignores a
	// profile, and its deny rules, for a thread given a sandbox mode.
	Permissions string

	// Cache says how a thread gets its session's cache directory
	// (adapter.StartSpec.CacheDir).
	Cache Cache
}

// Cache is how each thread is given its session's own writable cache
// directory, through per-thread config overrides in thread/start and
// thread/resume: the directory is added to what the sandbox may write,
// and Env's variables point at it in the thread's command environment.
type Cache struct {
	// Env are the variables to point at the directory (XDG_CACHE_HOME,
	// CCACHE_DIR): those the owner's config does not set already.
	Env []string
	// Roots are the legacy sandbox_workspace_write.writable_roots the
	// thread has without the override, which replaces them; RootsKnown
	// is false when the node could not read them, and then the directory
	// is not added under the legacy sandbox.
	Roots      []string
	RootsKnown bool
	// ProfileOK: under the permission profile, a per-thread
	// permissions.<profile>.filesystem table can add the directory
	// without replacing a table node.json sets.
	ProfileOK bool
}

// Adapter manages one app-server process and its threads.
type Adapter struct {
	cfg Config

	mu      sync.Mutex
	client  *codex.Client
	threads map[string]*handle
	early   map[string]*earlyNotes // notifications for threads not yet registered
	closed  bool
	restart sync.Mutex
	started time.Time // when client started
	quick   int       // app-servers in a row that exited within quickExit of starting
}

// Restart pacing; variables so tests can shorten them.
var (
	restartBackoff = time.Second      // first wait before a restart; doubles up to 30s
	quickExit      = 30 * time.Second // an app-server that exits sooner counts toward giving up
)

const maxRestarts = 8

type note struct {
	method string
	params json.RawMessage
}

// earlyNotes are notifications for a thread with no handle yet, kept from
// when the first arrived. Most are for a thread whose thread/start or
// thread/resume response is still in flight; the rest, for threads no
// handle will claim (one stopped while its notifications were on the way,
// a start that failed), are dropped after earlyTTL or to make room.
type earlyNotes struct {
	since time.Time
	notes []note
}

const (
	earlyTTL     = time.Minute
	earlyThreads = 64  // threads with notifications kept
	earlyPerTh   = 128 // notifications kept per thread
)

// New returns an adapter; app-server starts on first use.
func New(cfg Config) *Adapter {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Adapter{cfg: cfg, threads: map[string]*handle{}, early: map[string]*earlyNotes{}}
}

func (a *Adapter) Name() string { return Harness }

// Info returns the initialize response of the running app-server, if any.
func (a *Adapter) Info() (codex.InitializeResponse, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		return codex.InitializeResponse{}, false
	}
	return a.client.Info, true
}

func (a *Adapter) getClient(ctx context.Context) (*codex.Client, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, adapter.ErrClosed
	}
	if c := a.client; c != nil {
		select {
		case <-c.Done():
		default:
			a.mu.Unlock()
			return c, nil
		}
	}
	a.mu.Unlock()

	a.restart.Lock()
	defer a.restart.Unlock()
	a.mu.Lock()
	if c := a.client; c != nil {
		select {
		case <-c.Done():
		default:
			a.mu.Unlock()
			return c, nil
		}
	}
	a.mu.Unlock()
	c, err := codex.Start(ctx, codex.Options{
		Bin: a.cfg.Bin, Args: a.cfg.Args, Dir: a.cfg.Dir, Env: a.cfg.Env, Stderr: a.cfg.Stderr, Tap: a.cfg.Tap,
	}, a)
	if err != nil {
		// "connection closed" alone does not say why; app-server's last
		// words usually do. They stay in the node log, not in events.
		var se *codex.StartError
		if errors.As(err, &se) && se.Stderr != "" {
			a.cfg.Logf("codex app-server failed to start: %v; its stderr ends:\n%s", err, se.Stderr)
		}
		return nil, err
	}
	a.mu.Lock()
	a.client = c
	a.started = time.Now()
	a.mu.Unlock()
	a.cfg.Logf("codex app-server started (pid %d, codex home %s)", c.Pid(), c.Info.CodexHome)
	go a.watch(c)
	return c, nil
}

// watch restarts app-server after an unexpected exit and resumes threads.
func (a *Adapter) watch(c *codex.Client) {
	<-c.Done()
	a.mu.Lock()
	if a.closed || a.client != c {
		a.mu.Unlock()
		return
	}
	a.client = nil
	live := make([]*handle, 0, len(a.threads))
	for _, h := range a.threads {
		live = append(live, h)
	}
	lived := time.Since(a.started).Round(time.Second)
	if lived < quickExit {
		a.quick++
	} else {
		a.quick = 0
	}
	quick := a.quick
	a.mu.Unlock()
	// Its stdout closed; make sure the process and anything it started
	// are gone too.
	_ = c.Kill()
	how := fmt.Sprintf("%s after %s", exitStatus(c.ExitErr()), lived)
	for _, h := range live {
		h.lostClient()
	}
	// One that keeps exiting right after it starts would otherwise be
	// restarted every second for good: wait longer each time, and give up
	// after as many as for failed starts. A session started later still
	// gets one more app-server; if that one exits as quickly, it is not
	// restarted either.
	if quick > maxRestarts {
		a.cfg.Logf("codex app-server exited (%s), %d times in a row within %s of starting; not restarting it", how, quick, quickExit)
		for _, h := range live {
			if !h.isClosed() {
				h.setStatus(model.StatusError, "codex app-server keeps exiting right after it starts")
			}
		}
		return
	}
	a.cfg.Logf("codex app-server exited (%s); restarting and resuming %d thread(s)", how, len(live))
	backoff := restartBackoff
	if quick > 1 {
		time.Sleep(min(restartBackoff<<(quick-2), 30*time.Second))
	}
	for attempt := 0; attempt < maxRestarts; attempt++ {
		a.mu.Lock()
		closed := a.closed
		a.mu.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		nc, err := a.getClient(ctx)
		if err == nil {
			for _, h := range live {
				// A session stopped while app-server was down stays stopped.
				if err := h.resumeOn(ctx, nc); err != nil && !errors.Is(err, adapter.ErrClosed) {
					h.setStatus(model.StatusError, "resume after app-server restart failed: "+err.Error())
				}
			}
			cancel()
			return
		}
		cancel()
		a.cfg.Logf("codex app-server restart failed: %v", err)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	for _, h := range live {
		if !h.isClosed() {
			h.setStatus(model.StatusError, "codex app-server could not be restarted")
		}
	}
}

// exitStatus describes how a process ended. Windows status codes, such as
// 0xC000013A for a closed console, are in hex.
func exitStatus(err error) string {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "exit code 0"
	case errors.As(err, &ee) && ee.ExitCode() > 255:
		return fmt.Sprintf("exit code 0x%X", uint32(ee.ExitCode()))
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return fmt.Sprintf("exit code %d", ee.ExitCode())
	}
	return err.Error()
}

// Close stops app-server.
func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	c := a.client
	a.client = nil
	hs := make([]*handle, 0, len(a.threads))
	for _, h := range a.threads {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	var err error
	if c != nil {
		err = c.Close()
	}
	// app-server is gone, so no turn can still need its image files.
	for _, h := range hs {
		h.cleanImages()
	}
	return err
}

// OnNotification routes app-server notifications to sessions.
func (a *Adapter) OnNotification(method string, params json.RawMessage) {
	tid := codex.ThreadIDOf(params)
	if tid == "" {
		return
	}
	a.mu.Lock()
	h := a.threads[tid]
	if h == nil {
		// The thread/start response may still be in flight; keep a few
		// notifications so they can be replayed once the handle exists.
		a.keepEarlyLocked(tid, note{method, params}, time.Now())
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	h.onNote(method, params)
}

// keepEarlyLocked buffers n for thread tid, first dropping what is older
// than earlyTTL and, if every slot is still taken, the oldest thread's, so
// threads no handle claims cannot fill the buffer for good.
func (a *Adapter) keepEarlyLocked(tid string, n note, now time.Time) {
	q := a.early[tid]
	if q == nil {
		var oldest string
		for id, e := range a.early {
			if now.Sub(e.since) > earlyTTL {
				delete(a.early, id)
			} else if oldest == "" || e.since.Before(a.early[oldest].since) {
				oldest = id
			}
		}
		if len(a.early) >= earlyThreads {
			delete(a.early, oldest)
		}
		q = &earlyNotes{since: now}
		a.early[tid] = q
	}
	if len(q.notes) < earlyPerTh {
		q.notes = append(q.notes, n)
	}
}

// OnServerRequest handles approvals and rejects everything else.
func (a *Adapter) OnServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	a.mu.Lock()
	c := a.client
	h := a.threads[codex.ThreadIDOf(params)]
	a.mu.Unlock()
	if c == nil {
		return
	}
	if h != nil && (method == codex.RequestCommandApproval || method == codex.RequestFileChangeApproval) {
		h.onApproval(c, id, method, params)
		return
	}
	a.cfg.Logf("codex: rejecting unsupported server request %s", method)
	_ = c.ReplyError(id, jsonrpc.CodeMethodNotFound, "switchworks does not support "+method+" yet")
}

func (a *Adapter) register(h *handle) []note {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threads[h.threadID] = h
	q := a.early[h.threadID]
	delete(a.early, h.threadID)
	if q == nil {
		return nil
	}
	return q.notes
}

func (a *Adapter) unregister(h *handle) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.threads[h.threadID] == h {
		delete(a.threads, h.threadID)
	}
}

// Start creates a new thread.
func (a *Adapter) Start(ctx context.Context, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if err := proc.CheckPinned(spec.CwdRoot, spec.Cwd); err != nil {
		return nil, err
	}
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	sandbox, perms := a.sandboxFor(spec.Sandbox)
	resp, err := c.ThreadStart(ctx, codex.ThreadStartParams{
		Cwd:            spec.Cwd,
		Model:          spec.Model,
		ModelProvider:  spec.Provider,
		ApprovalPolicy: spec.ApprovalPolicy,
		Sandbox:        sandbox,
		Permissions:    perms,
		ServiceName:    "switchworks",
		Config:         a.threadConfig(spec),
	})
	if err != nil {
		return nil, fmt.Errorf("thread/start: %w", err)
	}
	h := newHandle(a, c, resp.Thread.ID, spec, sink)
	if err := h.attached(c, resp, false); err != nil {
		// Refused: leave the thread unwatched (its turns would run with
		// the wrong sandbox, so none is started).
		_ = c.ThreadUnsubscribe(ctx, resp.Thread.ID)
		return nil, err
	}
	if spec.Name != "" {
		if err := c.ThreadSetName(ctx, h.threadID, spec.Name); err != nil {
			a.cfg.Logf("codex: thread/name/set: %v", err)
		}
	}
	return h, nil
}

// sandboxFor returns the sandbox mode and permission profile to send for a
// session's sandbox: the configured profile for workspace-write, if any.
func (a *Adapter) sandboxFor(mode string) (sandbox, permissions string) {
	if mode == "workspace-write" && a.cfg.Permissions != "" {
		return "", a.cfg.Permissions
	}
	return mode, ""
}

// Resume reattaches to an existing thread.
func (a *Adapter) Resume(ctx context.Context, ref model.NativeRef, spec adapter.StartSpec, sink adapter.Sink) (adapter.Handle, error) {
	if ref.Harness != "" && ref.Harness != Harness {
		return nil, fmt.Errorf("codex adapter cannot resume %s session", ref.Harness)
	}
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	h := newHandle(a, c, ref.ID, spec, sink)
	if err := h.resumeOn(ctx, c); err != nil {
		return nil, err
	}
	return h, nil
}

type pendingApproval struct {
	rpcID     json.RawMessage
	turnID    string
	decisions []string // offered by Codex; empty = default set
}

type handle struct {
	a        *Adapter
	threadID string
	spec     adapter.StartSpec
	sink     adapter.Sink

	mu         sync.Mutex
	c          *codex.Client
	status     string
	idleSince  time.Time // when status last became idle
	activeTurn string
	turnSince  time.Time // when activeTurn was set
	doneTurns  []string  // recently completed turn ids (bounded)
	approvals  map[string]pendingApproval
	model      string
	provider   string
	home       string
	closed     bool
	// readOnly: the thread's effective sandbox, as app-server last
	// reported it, is read-only (Caps.ReadOnly).
	readOnly bool
	// refused is why the last thread/resume was refused (see attached):
	// no turn runs until a resume is accepted.
	refused error

	// commandItems are the commandExecution item ids of each running
	// turn, so an interrupt can stop the commands that turn left running;
	// stopping keeps an interrupted turn's ids until that is done.
	commandItems map[string][]string
	stopping     map[string]bool

	// Image files written for localImage inputs, by turn id; removed when
	// the turn completes (app-server has read them by then) or the
	// session stops. imageMeta maps each file to its attachment, so the
	// turn's userMessage item can show the image without its path.
	scratch    string
	ownScratch bool
	files      map[string][]string
	imageMeta  map[string]model.Attachment
}

func newHandle(a *Adapter, c *codex.Client, threadID string, spec adapter.StartSpec, sink adapter.Sink) *handle {
	return &handle{
		a: a, c: c, threadID: threadID, spec: spec, sink: sink,
		status: model.StatusStarting, approvals: map[string]pendingApproval{},
		home: c.Info.CodexHome, scratch: spec.ScratchDir,
		files: map[string][]string{}, imageMeta: map[string]model.Attachment{},
	}
}

func (h *handle) emit(kind string, payload any) {
	h.sink(adapter.Event{Kind: kind, Payload: payload})
}

func (h *handle) caps() model.Caps {
	h.mu.Lock()
	ro := h.readOnly
	h.mu.Unlock()
	return model.Caps{Message: true, Steer: true, Interrupt: true, Stop: true, Approve: true, Images: true, Policy: true, ReadOnly: ro}
}

// sandboxTypes maps the sandbox modes a session asks for to the policy
// type app-server reports for a thread that has it.
var sandboxTypes = map[string]string{
	"read-only": codex.SandboxReadOnly, "workspace-write": codex.SandboxWorkspaceWrite,
	"danger-full-access": codex.SandboxDangerFullAccess,
}

// sandboxRank orders sandbox policy types from the strictest. Under
// externalSandbox Codex leaves sandboxing to something outside it, so as
// far as Switchworks can tell it is full access.
var sandboxRank = map[string]int{
	codex.SandboxReadOnly: 0, codex.SandboxWorkspaceWrite: 1,
	codex.SandboxExternal: 2, codex.SandboxDangerFullAccess: 2,
}

// sandboxCheck is how the sandbox a thread runs with compares with the
// one its session asked for.
type sandboxCheck int

const (
	sandboxAsAsked  sandboxCheck = iota
	sandboxStricter              // less than asked for: degraded, but safe
	sandboxLooser                // more than asked for: refused
	sandboxUnknown               // not reported, or of a type not known here
)

// effectiveSandbox reads the sandbox policy a thread/start or
// thread/resume response reports and compares it with the requested mode,
// with what differs in mismatch. Under a permission profile (profile) the
// reported policy is only a legacy view of the profile, which reports a
// profile its legacy types cannot express as externalSandbox, so that
// counts as asked for there.
func effectiveSandbox(requested string, profile bool, reported json.RawMessage) (readOnly bool, check sandboxCheck, mismatch string) {
	want, known := sandboxTypes[requested]
	var p codex.SandboxPolicy
	if len(reported) == 0 || json.Unmarshal(reported, &p) != nil || p.Type == "" {
		if !known || want == codex.SandboxDangerFullAccess {
			return requested == "read-only", sandboxAsAsked, "" // nothing is looser than full access
		}
		return requested == "read-only", sandboxUnknown, "codex did not report the sandbox it runs this thread with"
	}
	readOnly = p.Type == codex.SandboxReadOnly
	got, ok := sandboxRank[p.Type]
	switch {
	case !known || p.Type == want:
		return readOnly, sandboxAsAsked, ""
	case profile && want == codex.SandboxWorkspaceWrite && p.Type == codex.SandboxExternal:
		return readOnly, sandboxAsAsked, ""
	case !ok:
		if want == codex.SandboxDangerFullAccess {
			return readOnly, sandboxAsAsked, ""
		}
		return readOnly, sandboxUnknown, fmt.Sprintf("codex runs this thread with sandbox %q, which Switchworks does not know", p.Type)
	case got > sandboxRank[want]:
		return readOnly, sandboxLooser, fmt.Sprintf("codex runs this thread with sandbox %s, looser than the %s it was started with", sandboxName(p.Type), requested)
	case got < sandboxRank[want]:
		return readOnly, sandboxStricter, fmt.Sprintf("codex runs this thread with sandbox %s, not the %s it was started with", sandboxName(p.Type), requested)
	}
	return readOnly, sandboxAsAsked, ""
}

// sandboxName names a sandbox policy type the way sessions name modes.
func sandboxName(typ string) string {
	for mode, t := range sandboxTypes {
		if t == typ {
			return mode
		}
	}
	if typ == codex.SandboxExternal {
		return "external"
	}
	return typ
}

// attached finishes registration after thread/start or thread/resume. It
// returns ErrClosed, registering nothing, when the session was stopped
// meanwhile (app-server restarted under a session being stopped).
//
// It also refuses, registering nothing, a thread Codex runs with a looser
// sandbox than asked for, and one whose sandbox it does not report (or
// reports as a type not known here) when an agent spawned the session; a
// person's session then only gets a warning.
func (h *handle) attached(c *codex.Client, resp *codex.ThreadStartResponse, resumed bool) error {
	// What the thread really runs with: Codex may not give it the sandbox
	// asked for (on Windows, workspace-write without a Windows sandbox
	// mode runs read-only), and says so only here.
	_, perms := h.a.sandboxFor(h.spec.Sandbox)
	ro, check, mismatch := effectiveSandbox(h.spec.Sandbox, perms != "", resp.Sandbox)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return adapter.ErrClosed
	}
	if check == sandboxLooser || check == sandboxUnknown && (h.spec.ByAgent || h.spec.CwdRoot != "") {
		h.refused = fmt.Errorf("codex: refusing thread %s: %s", h.threadID, mismatch)
		h.c = nil
		h.mu.Unlock()
		h.a.cfg.Logf("%v", h.refused)
		return h.refused
	}
	h.refused = nil
	h.c = c
	h.model = resp.Model
	h.provider = resp.ModelProvider
	h.home = c.Info.CodexHome
	h.activeTurn = ""
	h.readOnly = ro
	h.mu.Unlock()
	early := h.a.register(h)
	// Stop marks the handle closed before it unregisters, so a Stop that
	// raced past the check above is seen here.
	if h.isClosed() {
		h.a.unregister(h)
		return adapter.ErrClosed
	}
	h.emit(model.KindSessionRegistered, model.SessionRegistered{
		NativeRef: h.Ref(), Cwd: resp.Cwd, Model: resp.Model, Provider: resp.ModelProvider,
		Caps: h.caps(), Resumed: resumed,
	})
	st := model.StatusIdle
	if resp.Thread.Status != nil {
		st = mapThreadStatus(*resp.Thread.Status)
	}
	detail := ""
	if mismatch != "" {
		// Degraded, not failed: the session still runs, with less than it
		// was given, or (a person's session) with a sandbox not known.
		h.a.cfg.Logf("codex: thread %s: sandbox degraded: %s", h.threadID, mismatch)
		h.emit(model.KindWarning, model.ErrorEvent{Message: "sandbox degraded: " + mismatch})
		detail = "degraded: " + mismatch
	}
	h.setStatus(st, detail)
	for _, n := range early {
		h.onNote(n.method, n.params)
	}
	return nil
}

func (h *handle) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *handle) resumeOn(ctx context.Context, c *codex.Client) error {
	if h.isClosed() {
		return adapter.ErrClosed
	}
	if err := proc.CheckPinned(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	h.mu.Lock()
	policy := h.spec.ApprovalPolicy
	h.mu.Unlock()
	sandbox, perms := h.a.sandboxFor(h.spec.Sandbox)
	resp, err := c.ThreadResume(ctx, codex.ThreadResumeParams{
		ThreadID:       h.threadID,
		ApprovalPolicy: policy,
		Sandbox:        sandbox,
		Permissions:    perms,
		ExcludeTurns:   true,
		Config:         h.a.threadConfig(h.spec),
	})
	if err != nil {
		return fmt.Errorf("thread/resume %s: %w", h.threadID, err)
	}
	if err := h.attached(c, resp, true); err != nil {
		// Stopped while resuming: drop the subscription thread/resume made.
		_ = c.ThreadUnsubscribe(ctx, h.threadID)
		return err
	}
	return nil
}

// lostClient ends what app-server took with it: the running turn fails,
// its pending approvals are cleared, and image files and command ids no
// turn will need are dropped.
func (h *handle) lostClient() {
	h.mu.Lock()
	h.c = nil
	turn := h.activeTurn
	h.activeTurn = ""
	if turn != "" {
		h.doneTurns = append(h.doneTurns, turn)
		if len(h.doneTurns) > 16 {
			h.doneTurns = h.doneTurns[len(h.doneTurns)-16:]
		}
	}
	var cleared []string
	for id := range h.approvals {
		cleared = append(cleared, id)
		delete(h.approvals, id)
	}
	var files []string
	for id, ps := range h.files {
		files = append(files, ps...)
		delete(h.files, id)
	}
	h.commandItems = nil
	h.stopping = nil
	h.mu.Unlock()
	h.removeFiles(files)
	for _, aid := range cleared {
		h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: aid, Decision: "cleared"})
	}
	if turn != "" {
		h.emit(model.KindTurnCompleted, model.TurnCompleted{TurnID: turn, Status: "failed", Error: "codex app-server exited"})
	}
	h.setStatus(model.StatusError, "codex app-server exited; restarting")
}

func (h *handle) client() (*codex.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, adapter.ErrClosed
	}
	if h.c == nil {
		if h.refused != nil {
			return nil, h.refused
		}
		return nil, errors.New("codex app-server is restarting")
	}
	return h.c, nil
}

func (h *handle) Ref() model.NativeRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	return model.NativeRef{Harness: Harness, ID: h.threadID, Home: h.home}
}

func (h *handle) Caps() model.Caps { return h.caps() }

// SetApprovalPolicy changes the policy the next turn starts with; a turn
// already running keeps its own.
func (h *handle) SetApprovalPolicy(_ context.Context, policy string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return adapter.ErrClosed
	}
	h.spec.ApprovalPolicy = policy
	return nil
}

func (h *handle) Status() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *handle) setStatus(s, detail string) {
	h.mu.Lock()
	if h.status == s && detail == "" {
		h.mu.Unlock()
		return
	}
	if s == model.StatusIdle && h.status != s {
		h.idleSince = time.Now()
	}
	h.status = s
	h.mu.Unlock()
	h.emit(model.KindSessionStatus, model.SessionStatus{Status: s, Detail: detail})
}

func mapThreadStatus(s codex.ThreadStatus) string {
	switch s.Type {
	case "idle":
		return model.StatusIdle
	case "active":
		for _, f := range s.ActiveFlags {
			if f == "waitingOnApproval" {
				return model.StatusWaitingApproval
			}
		}
		return model.StatusActive
	case "systemError":
		return model.StatusError
	case "notLoaded":
		return model.StatusGone
	}
	return model.StatusIdle
}

func (h *handle) onNote(method string, params json.RawMessage) {
	switch method {
	case codex.NoteThreadStatusChanged:
		var n codex.ThreadStatusChangedNotification
		if json.Unmarshal(params, &n) == nil {
			st := mapThreadStatus(n.Status)
			if st == model.StatusIdle {
				// An idle report after the turn started counts for
				// dropStaleTurnLocked even when the status was idle already.
				h.mu.Lock()
				if h.activeTurn != "" && !h.idleSince.After(h.turnSince) {
					h.idleSince = time.Now()
				}
				h.mu.Unlock()
			}
			h.setStatus(st, "")
		}
	case codex.NoteThreadClosed:
		h.setStatus(model.StatusGone, "thread closed by codex")
	case codex.NoteTurnStarted:
		var n codex.TurnNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			h.activeTurn = n.Turn.ID
			h.turnSince = time.Now()
			h.mu.Unlock()
			h.emit(model.KindTurnStarted, model.TurnStarted{TurnID: n.Turn.ID})
		}
	case codex.NoteTurnCompleted:
		var n codex.TurnNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			if h.activeTurn == n.Turn.ID {
				h.activeTurn = ""
			}
			h.doneTurns = append(h.doneTurns, n.Turn.ID)
			if len(h.doneTurns) > 16 {
				h.doneTurns = h.doneTurns[len(h.doneTurns)-16:]
			}
			done := h.files[n.Turn.ID]
			delete(h.files, n.Turn.ID)
			if !h.stopping[n.Turn.ID] {
				delete(h.commandItems, n.Turn.ID)
			}
			h.mu.Unlock()
			h.removeFiles(done)
			tc := model.TurnCompleted{TurnID: n.Turn.ID, Status: n.Turn.Status}
			if n.Turn.Error != nil {
				tc.Error = n.Turn.Error.Message
			}
			if n.Turn.DurationMs != nil {
				tc.DurationMs = *n.Turn.DurationMs
			}
			h.emit(model.KindTurnCompleted, tc)
		}
	case codex.NoteItemStarted, codex.NoteItemCompleted:
		var n codex.ItemNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		var hdr codex.ItemHeader
		_ = json.Unmarshal(n.Item, &hdr)
		if method == codex.NoteItemStarted && hdr.Type == "commandExecution" && n.TurnID != "" {
			h.mu.Lock()
			if h.commandItems == nil {
				h.commandItems = map[string][]string{}
			}
			h.commandItems[n.TurnID] = append(h.commandItems[n.TurnID], hdr.ID)
			h.mu.Unlock()
		}
		it := model.Item{TurnID: n.TurnID, ItemID: hdr.ID, ItemType: hdr.Type, Raw: n.Item}
		if hdr.Type == "userMessage" {
			it.Raw = h.userItem(n.Item)
		}
		if hdr.Type == "agentMessage" {
			var am codex.AgentMessageItem
			if json.Unmarshal(n.Item, &am) == nil {
				it.Text = am.Text
			}
		}
		kind := model.KindItemStarted
		if method == codex.NoteItemCompleted {
			kind = model.KindItemCompleted
		}
		h.emit(kind, it)
	case codex.NoteAgentMessageDelta:
		var n codex.AgentMessageDeltaNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindItemDelta, model.ItemDelta{TurnID: n.TurnID, ItemID: n.ItemID, Text: n.Delta})
		}
	case codex.NoteTurnDiffUpdated:
		var n codex.TurnDiffUpdatedNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindTurnDiff, model.TurnDiff{TurnID: n.TurnID, Diff: n.Diff})
		}
	case codex.NoteTokenUsageUpdated:
		var n codex.ThreadTokenUsageUpdatedNotification
		if json.Unmarshal(params, &n) == nil {
			h.mu.Lock()
			m, p := h.model, h.provider
			h.mu.Unlock()
			ev := model.UsageEvent{TurnID: n.TurnID, Model: m, Provider: p,
				Last: usage(n.TokenUsage.Last), Total: usage(n.TokenUsage.Total)}
			if n.TokenUsage.ModelContextWindow != nil {
				ev.ContextWindow = *n.TokenUsage.ModelContextWindow
			}
			h.emit(model.KindUsage, ev)
		}
	case codex.NoteError:
		var n codex.ErrorNotification
		if json.Unmarshal(params, &n) == nil {
			h.emit(model.KindError, model.ErrorEvent{TurnID: n.TurnID, Message: n.Error.Message, WillRetry: n.WillRetry})
		}
	case codex.NoteRequestResolved:
		var n codex.ServerRequestResolvedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		h.mu.Lock()
		var cleared []string
		for aid, p := range h.approvals {
			if string(p.rpcID) == string(n.RequestID) {
				cleared = append(cleared, aid)
				delete(h.approvals, aid)
			}
		}
		h.mu.Unlock()
		for _, aid := range cleared {
			h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: aid, Decision: "cleared"})
		}
	}
}

func usage(b codex.TokenUsageBreakdown) model.Usage {
	return model.Usage{
		InputTokens: b.InputTokens, CachedInputTokens: b.CachedInputTokens, OutputTokens: b.OutputTokens,
		ReasoningTokens: b.ReasoningOutputTokens, TotalTokens: b.TotalTokens,
	}
}

// threadConfig is the per-thread config override for a session: its
// fleet tools (mcpConfig) and its cache directory (cacheConfig).
func (a *Adapter) threadConfig(spec adapter.StartSpec) map[string]any {
	out := mcpConfig(spec.MCP)
	for k, v := range a.cacheConfig(spec) {
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

// cacheConfig renders the overrides that give a thread its session's
// cache directory: writable (a write entry in the permission profile's
// filesystem table, or one more legacy writable root), with Cache.Env
// pointing at it. Codex applies a dotted key of a per-thread override
// like a -c one, merging it into the tables of config.toml; an array
// (writable_roots) is replaced, hence Cache.Roots. Read-only sessions
// get nothing: they could not write it. When it cannot be made writable
// the variables are still set, so tools fail as they would on ~/.cache.
func (a *Adapter) cacheConfig(spec adapter.StartSpec) map[string]any {
	dir := spec.CacheDir
	if dir == "" || spec.Sandbox == "read-only" || spec.Sandbox == "" {
		return nil
	}
	c := a.cfg.Cache
	out := map[string]any{}
	if spec.Sandbox == "workspace-write" {
		switch _, perms := a.sandboxFor(spec.Sandbox); {
		case perms != "" && c.ProfileOK:
			// Codex profile paths are written with forward slashes on
			// every OS (docs/harnesses.md).
			out["permissions."+perms+".filesystem"] = map[string]any{filepath.ToSlash(dir): "write"}
		case perms == "" && c.RootsKnown:
			roots := make([]any, 0, len(c.Roots)+1)
			for _, r := range c.Roots {
				if r != dir {
					roots = append(roots, r)
				}
			}
			out["sandbox_workspace_write.writable_roots"] = append(roots, dir)
		}
	}
	for _, v := range c.Env {
		out["shell_environment_policy.set."+v] = dir
	}
	return out
}

// mcpConfig renders a per-thread config override that attaches an MCP
// server to this thread only. Its tools are auto-approved
// (default_tools_approval_mode = "approve"): Codex otherwise asks before
// every MCP call, which fails outright under approval policy "never".
// Switchworks applies its own guards to fleet operations.
func mcpConfig(m *adapter.MCPServer) map[string]any {
	if m == nil {
		return nil
	}
	timeout := m.ToolTimeout
	if timeout <= 0 {
		timeout = 60
	}
	args := m.Args
	if args == nil {
		args = []string{}
	}
	return map[string]any{"mcp_servers": map[string]any{m.Name: map[string]any{
		"command":                     m.Command,
		"args":                        args,
		"env":                         m.Env,
		"startup_timeout_sec":         30,
		"tool_timeout_sec":            timeout,
		"default_tools_approval_mode": "approve",
	}}}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *handle) onApproval(c *codex.Client, id json.RawMessage, method string, params json.RawMessage) {
	aid := ulid.New()
	req := model.ApprovalRequested{ApprovalID: aid}
	switch method {
	case codex.RequestCommandApproval:
		var p codex.CommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchworks: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID = "command", p.TurnID, p.ItemID
		req.Command, req.Cwd, req.Reason = deref(p.Command), deref(p.Cwd), deref(p.Reason)
		req.Decisions = codex.DecisionNames(p.AvailableDecisions)
	case codex.RequestFileChangeApproval:
		var p codex.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = c.ReplyError(id, jsonrpc.CodeInvalidParams, "switchworks: bad approval params")
			return
		}
		req.Kind, req.TurnID, req.ItemID, req.Reason = "file_change", p.TurnID, p.ItemID, deref(p.Reason)
		req.GrantRoot = deref(p.GrantRoot)
	}
	h.mu.Lock()
	h.approvals[aid] = pendingApproval{rpcID: append(json.RawMessage(nil), id...), turnID: req.TurnID, decisions: req.Decisions}
	h.mu.Unlock()
	h.emit(model.KindApprovalRequested, req)
	h.setStatus(model.StatusWaitingApproval, "")
}

// Resolve answers a pending approval.
func (h *handle) Resolve(ctx context.Context, approvalID, decision string) error {
	switch decision {
	case codex.DecisionAccept, codex.DecisionAcceptForSession, codex.DecisionDecline, codex.DecisionCancel:
	default:
		return fmt.Errorf("unknown decision %q (accept, acceptForSession, decline, cancel)", decision)
	}
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	p, ok := h.approvals[approvalID]
	if ok && len(p.decisions) > 0 && !containsStr(p.decisions, decision) {
		h.mu.Unlock()
		return fmt.Errorf("decision %q not offered for this prompt (offered: %v)", decision, p.decisions)
	}
	if ok {
		delete(h.approvals, approvalID)
	}
	h.mu.Unlock()
	if !ok {
		return adapter.ErrUnknown
	}
	if err := c.Reply(p.rpcID, codex.ApprovalResponse{Decision: decision}); err != nil {
		return err
	}
	h.emit(model.KindApprovalResolved, model.ApprovalResolved{ApprovalID: approvalID, Decision: decision})
	return nil
}

// Deliver starts a turn (queue) or steers the active one.
func (h *handle) Deliver(ctx context.Context, in adapter.Input, mode adapter.DeliveryMode) error {
	// Codex resolves the cwd for each command, so check it before every
	// turn and steer, not only when the thread starts.
	if err := proc.CheckPinned(h.spec.CwdRoot, h.spec.Cwd); err != nil {
		return err
	}
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.dropStaleTurnLocked()
	active := h.activeTurn
	status := h.status
	policy := h.spec.ApprovalPolicy
	h.mu.Unlock()
	switch mode {
	case adapter.DeliverSteer:
		if active == "" {
			return adapter.ErrNotActive
		}
		input, paths, err := h.inputs(in)
		if err != nil {
			return err
		}
		_, err = c.TurnSteer(ctx, codex.TurnSteerParams{
			ThreadID: h.threadID, ExpectedTurnID: active, Input: input,
			ClientUserMessageID: in.ClientMessageID,
		})
		h.adoptFiles(active, paths, err)
		return err
	default:
		if active != "" || status == model.StatusActive || status == model.StatusWaitingApproval {
			return adapter.ErrBusy
		}
		input, paths, err := h.inputs(in)
		if err != nil {
			return err
		}
		// Every turn carries the policy, so a change (SetApprovalPolicy)
		// takes effect with the next one.
		resp, err := c.TurnStart(ctx, codex.TurnStartParams{
			ThreadID: h.threadID, Input: input, ClientUserMessageID: in.ClientMessageID,
			ApprovalPolicy: policy,
		})
		if err != nil {
			h.adoptFiles("", paths, err)
			return err
		}
		// Notifications for this turn may already have been processed (a
		// fast turn can complete before the turn/start response is read
		// here), so only adopt it if it has not finished.
		h.mu.Lock()
		if resp.Turn.Status == codex.TurnInProgress && h.activeTurn == "" && !containsStr(h.doneTurns, resp.Turn.ID) {
			h.activeTurn = resp.Turn.ID
			h.turnSince = time.Now()
		}
		h.mu.Unlock()
		h.adoptFiles(resp.Turn.ID, paths, nil)
		return nil
	}
}

// staleTurnAfter is how long the thread must have been idle before a turn
// id the handle still holds counts as stale; a variable so tests can
// shorten it.
var staleTurnAfter = 10 * time.Second

// dropStaleTurnLocked forgets the active turn when the thread has been idle
// for a while since the turn started: its turn/completed never came (a
// compaction or interrupted turn, say). Holding on to it would refuse every
// new turn with ErrBusy, stalling the session's queue for good. app-server
// reports the thread idle just before turn/completed, so a fresh idle
// status is not enough; and an idle status from before the turn started
// (turn/started can be read before the thread's switch to active) says
// nothing about the turn, which may well be running: forgetting it would
// start a second turn beside it, which Interrupt and Stop then miss.
func (h *handle) dropStaleTurnLocked() {
	if h.activeTurn == "" || h.status != model.StatusIdle || !h.idleSince.After(h.turnSince) ||
		time.Since(h.idleSince) < staleTurnAfter {
		return
	}
	h.a.cfg.Logf("codex: thread %s has been idle for %s but turn %s never completed; forgetting it",
		h.threadID, time.Since(h.idleSince).Round(time.Second), h.activeTurn)
	h.doneTurns = append(h.doneTurns, h.activeTurn)
	if len(h.doneTurns) > 16 {
		h.doneTurns = h.doneTurns[len(h.doneTurns)-16:]
	}
	h.activeTurn = ""
}

// Interrupt cancels the active turn. app-server keeps a command the turn
// was running alive as a background terminal, so the commands this turn
// started are stopped too; ones from earlier turns (a dev server, say)
// keep running.
func (h *handle) Interrupt(ctx context.Context) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	h.mu.Lock()
	active := h.activeTurn
	if active != "" {
		if h.stopping == nil {
			h.stopping = map[string]bool{}
		}
		h.stopping[active] = true
	}
	h.mu.Unlock()
	if active == "" {
		return adapter.ErrNotActive
	}
	if err := c.TurnInterrupt(ctx, h.threadID, active); err != nil {
		h.mu.Lock()
		delete(h.stopping, active)
		h.mu.Unlock()
		return err
	}
	go h.stopTurnCommands(c, active)
	return nil
}

// stopTurnCommands terminates the background terminals started by the
// interrupted turn's commandExecution items, including ones that start
// while the turn winds down. A command becomes a background terminal as
// the turn ends, so the list is checked twice. Older app-servers without
// the background terminal methods are left as they are.
func (h *handle) stopTurnCommands(c *codex.Client, turnID string) {
	defer func() {
		h.mu.Lock()
		delete(h.stopping, turnID)
		delete(h.commandItems, turnID)
		h.mu.Unlock()
	}()
	for _, wait := range []time.Duration{500 * time.Millisecond, 3 * time.Second} {
		time.Sleep(wait)
		h.mu.Lock()
		items := append([]string(nil), h.commandItems[turnID]...)
		h.mu.Unlock()
		if len(items) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		terms, err := c.BackgroundTerminals(ctx, h.threadID)
		if err != nil {
			cancel()
			h.a.cfg.Logf("codex: list background terminals of %s: %v", h.threadID, err)
			return
		}
		for _, bt := range terms {
			if containsStr(items, bt.ItemID) {
				if err := c.TerminateBackgroundTerminal(ctx, h.threadID, bt.ProcessID); err != nil {
					h.a.cfg.Logf("codex: stop interrupted command %q: %v", bt.Command, err)
				}
			}
		}
		cancel()
	}
}

// Stop interrupts any active turn and unsubscribes from the thread.
func (h *handle) Stop(ctx context.Context) error {
	// Closed first, so a resume after an app-server restart that is under
	// way sees it (see attached) and leaves the session stopped.
	h.mu.Lock()
	c, active, was := h.c, h.activeTurn, h.closed
	h.closed = true
	h.mu.Unlock()
	if was {
		return nil
	}
	if c != nil {
		if active != "" {
			_ = c.TurnInterrupt(ctx, h.threadID, active)
		}
		// Nothing the session started should outlive it. An interrupted
		// command becomes a background terminal as its turn ends, which
		// may be after this first clean, so clean again shortly after.
		if err := c.CleanBackgroundTerminals(ctx, h.threadID); err != nil {
			h.a.cfg.Logf("codex: clean background terminals of %s: %v", h.threadID, err)
		} else if active != "" {
			go func() {
				for _, wait := range []time.Duration{time.Second, 3 * time.Second} {
					time.Sleep(wait)
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = c.CleanBackgroundTerminals(cctx, h.threadID)
					cancel()
				}
			}()
		}
		_ = c.ThreadUnsubscribe(ctx, h.threadID)
	}
	h.a.unregister(h)
	h.setStatus(model.StatusStopped, "")
	h.cleanImages()
	return nil
}
