# M0 findings (Codex 0.160.0, linux/amd64)

Evidence comes from running the real `codex` binary in the build sandbox.
Raw traffic is in `internal/codex/testdata/codex-0.160.0-session.jsonl`,
recorded by `TestRealCodex`.

## Answered

**Does a non-OpenAI provider from `config.toml` work through app-server?**
Yes, if the provider speaks the OpenAI Responses API. A `model_providers`
entry with `wire_api = "responses"` and `requires_openai_auth = false` drives
`thread/start` + `turn/start` end to end. Streaming deltas, tool calls,
approvals and token usage (`thread/tokenUsage/updated`, including
`cachedInputTokens`) all work. Exercised by `TestRealCodex` and
`swyd dev mock-provider`.

**`wire_api = "chat"` is gone.** Codex 0.160.0 refuses to load it: "`wire_api
= "chat"` is no longer supported" (openai/codex discussion #7782). Any GLM,
DeepSeek or MiMo endpoint used through Codex must be Responses-compatible, or
sit behind a translating proxy. This needs checking per provider before
making Codex the default harness.

**Does the interactive TUI expose a control socket (observed mode, M4)?**
Yes, indirectly, and better than expected. In 0.160.0 the TUI does not embed
app-server. It starts a shared, long-lived **app-server daemon** per
`CODEX_HOME`:

- `codex app-server --listen unix:// --managed…`, run from a Codex-managed
  copy under `$CODEX_HOME/packages/app-server-daemon/releases/<version>/`,
  with an updater process alongside;
- `$CODEX_HOME/app-server-control/app-server-control.sock` is a symlink to a
  socket under `/tmp/codex-daemon-<uid>/`;
- the TUI talks to it over that socket;
- the daemon keeps running after the TUI exits;
- Codex keeps per-thread writer locks in `$CODEX_HOME/thread-writer-locks/`.

Consequences:

1. Observed mode can attach to that daemon and watch live TUI threads,
   instead of tailing rollout files. The transport is WebSocket framing over
   the Unix socket (`codex app-server proxy` bridges raw bytes but does not
   do the framing). A minimal RFC 6455 client is about 200 lines of stdlib
   Go.
2. Option worth deciding before M4: point the node's managed sessions at the
   same daemon instead of a private `codex app-server` child. Sessions
   started by Switchyard would then appear in `codex resume` and vice
   versa, and Codex's writer locks would arbitrate the two-writers problem.
   The cost is that the daemon is Codex-managed and **self-updating**, so
   the protocol version can change under a running node. That makes the
   contract test and version pinning more important, not less.

**Approval prompts declare their allowed decisions.** With the experimental
API, `item/commandExecution/requestApproval` carries `availableDecisions`. For
a plain command it offered `accept`, `acceptWithExecpolicyAmendment` and
`cancel`, with **no `decline`**. Switchyard surfaces the list and rejects
decisions that were not offered. The schema had to be generated with
`--experimental` to include this field.

**Other protocol facts that differ from the README.**

- Requests with no parameters must still send `"params": {}`; omitting the
  field is an error.
- Server-initiated request ids start at 0.
- A fast turn can finish (`turn/completed` processed) before the
  `turn/start` response reaches the caller. Clients must not treat the
  response's `inProgress` turn as active without checking. This caused a
  real wedge, now fixed and covered.
- Every notification carries a top-level `emittedAtMs`.
- `thread/queue/*` methods exist (a server-side message queue). Worth
  evaluating for M2 instead of the node's own queue.

## Not answered here (needs your machines)

- **Windows 11**: whether `codex app-server` over stdio, the job-object
  teardown, and the daemon's Unix socket (AF_UNIX exists on Windows 10 1803+)
  all behave. Run `swyd dev run` on winbox first.
- **macOS and Fedora aarch64**: binaries cross-compile but were not run.
- **Antigravity**: no `agy` binary or Google sign-in was available. The
  adapter follows the documented headless flags only. Record a real stream
  with `swyd dev run --harness agy --record agy.jsonl "hello"` and send it
  back to pin the `step_update` schema.
