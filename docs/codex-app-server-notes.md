# Codex app-server: the subset Switchworks uses

Source of truth: `schema/codex/0.160.0/`, generated with
`codex app-server generate-json-schema --experimental`, plus recorded
traffic in `internal/codex/testdata/`. `internal/codex/contract_test.go`
fails if anything below stops matching the schema.

## Transport

`codex app-server` (stdio is the default): JSON-RPC 2.0, one JSON object per
line, **no `"jsonrpc"` field**. Both sides send requests. Error `-32001`
("Server overloaded; retry later.") is retried with exponential backoff and
jitter. The WebSocket listener is marked experimental and unsupported, so it
is not used.

## Handshake

1. `initialize` `{clientInfo: {name: "switchworks", title, version}, capabilities: {experimentalApi: true}}`
   returns `{userAgent, codexHome, platformFamily, platformOs}`.
2. Notification `initialized` (no params).

## Client requests

| Method | Used for | Notes |
| --- | --- | --- |
| `thread/start` | new session | `cwd, model, modelProvider, approvalPolicy (untrusted, on-request, never), sandbox (read-only, workspace-write, danger-full-access), serviceName`; returns `thread` plus effective `model`, `modelProvider` and `cwd` |
| `thread/resume` | node restart, app-server crash | `threadId, excludeTurns: true` |
| `thread/name/set` | label the thread with the session path | |
| `thread/unsubscribe` | stop | thread unloads after 30 min idle with no subscribers |
| `thread/list`, `thread/read` | observing sessions | typed, not yet used |
| `turn/start` | queue delivery | `threadId, input: [{type: "text", text}], clientUserMessageId` |
| `turn/steer` | steer delivery | needs `expectedTurnId` = the active turn |
| `turn/interrupt` | interrupt | `threadId, turnId`; the turn ends with status `interrupted` |

## Notifications mapped to events

| Codex | Switchworks event |
| --- | --- |
| `thread/status/changed` (`idle`, `active[activeFlags]`, `systemError`, `notLoaded`) | `session.status` (`waitingOnApproval` becomes `waiting_approval`) |
| `thread/closed` | `session.status` gone |
| `turn/started`, `turn/completed` | `turn.started`, `turn.completed` (status, error, durationMs) |
| `item/started`, `item/completed` | `item.started`, `item.completed` (agentMessage text extracted; raw item kept) |
| `item/agentMessage/delta` | `item.delta` (coalesced by the node, about 250 ms) |
| `turn/diff/updated` | `turn.diff` |
| `thread/tokenUsage/updated` | `usage` (`last` and cumulative `total`, incl. `cachedInputTokens`, `modelContextWindow`) |
| `error` | `error` (`willRetry`) |
| `serverRequest/resolved` | clears a parked approval (`approval.resolved` decision `cleared`) |

## Server requests

- `item/commandExecution/requestApproval` `{threadId, turnId, itemId, command, cwd, reason, availableDecisions}`
- `item/fileChange/requestApproval` `{threadId, turnId, itemId, reason, grantRoot}`

Both are answered with `{decision}`, one of `accept`, `acceptForSession`,
`decline` or `cancel`, limited to `availableDecisions` when that is present.
All other server requests (`item/tool/requestUserInput`,
`mcpServer/elicitation/request`, `item/permissions/requestApproval`,
`item/tool/call`, auth refresh, attestation, the legacy v1 approvals) are
rejected with `-32601`, so the turn never hangs waiting on Switchworks.

## Experimental surface in use

`experimentalApi: true` is required for `availableDecisions`. Nothing else
experimental is relied on.

## Behavior worth knowing

Observed with Codex 0.160.0 on Linux; the raw traffic is in
`internal/codex/testdata/codex-0.160.0-session.jsonl`.

- **Providers must speak the Responses API.** `wire_api = "chat"` is
  rejected at load time. A `model_providers` entry with
  `wire_api = "responses"` and `requires_openai_auth = false` works end to
  end: streaming, tool calls, approvals and token usage.
- **Approval prompts declare their decisions.** With the experimental API,
  `item/commandExecution/requestApproval` carries `availableDecisions`. A
  plain command may offer `accept`, `acceptWithExecpolicyAmendment` and
  `cancel` with no `decline`. Switchworks shows that list and rejects other
  decisions. The schema must be generated with `--experimental` to include
  this field.
- Requests with no parameters must still send `"params": {}`.
- Server-initiated request ids start at 0.
- A fast turn can complete before the `turn/start` response arrives, so a
  client must not treat the response's `inProgress` turn as active without
  checking.
- Every notification carries a top-level `emittedAtMs`.
- `thread/queue/*` methods exist (a server-side message queue); Switchworks
  keeps its own queue on the node.
- **The sandbox a thread gets can differ from the one asked for.** The
  `thread/start` and `thread/resume` responses carry `sandbox`, the
  effective legacy `SandboxPolicy` (`type`: `readOnly`, `workspaceWrite`,
  `dangerFullAccess`, `externalSandbox`). Switchworks compares it with the
  requested mode, reports a mismatch as a degraded session, and sets
  `caps.read_only` from it. Under a permission profile it is only a legacy
  view of the profile, so only `readOnly` counts as a mismatch there.
- **Codex 0.163 on Windows** runs a `workspace-write` thread read-only when
  `config.toml` sets no `[windows] sandbox` mode, without an error; under
  approval `never` every command is then rejected ("blocked by policy").
  `-c windows.sandbox="unelevated"` fixes it; the node passes that unless
  a Windows sandbox mode is configured.

## Codex's shared app-server daemon

The interactive TUI does not embed app-server. It starts a shared,
long-lived daemon per `CODEX_HOME`:

- `codex app-server --listen unix:// --managed…`, run from a Codex-managed
  copy under `$CODEX_HOME/packages/app-server-daemon/releases/<version>/`,
  with an updater process alongside;
- `$CODEX_HOME/app-server-control/app-server-control.sock` links to a socket
  under `/tmp/codex-daemon-<uid>/`, carrying WebSocket framing over the Unix
  socket;
- the daemon keeps running after the TUI exits, and per-thread writer locks
  live in `$CODEX_HOME/thread-writer-locks/`.

Switchworks runs its own private `codex app-server` per node instead. Pointing
nodes at the shared daemon would let them observe sessions started in the
TUI (and make Switchworks's sessions visible to `codex resume`), at the cost
of a self-updating daemon whose protocol can change under a running node.
