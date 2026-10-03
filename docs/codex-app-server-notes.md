# Codex app-server: the subset Switchyard uses

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

1. `initialize` `{clientInfo: {name: "switchyard", title, version}, capabilities: {experimentalApi: true}}`
   returns `{userAgent, codexHome, platformFamily, platformOs}`.
2. Notification `initialized` (no params).

## Client requests

| Method | Used for | Notes |
| --- | --- | --- |
| `thread/start` | new session | `cwd, model, modelProvider, approvalPolicy (untrusted, on-request, never), sandbox (read-only, workspace-write, danger-full-access), serviceName`; returns `thread` plus effective `model`, `modelProvider` and `cwd` |
| `thread/resume` | node restart, app-server crash | `threadId, excludeTurns: true` |
| `thread/name/set` | label the thread with the session path | |
| `thread/unsubscribe` | stop | thread unloads after 30 min idle with no subscribers |
| `thread/list`, `thread/read` | M4 discovery | typed, not yet used |
| `turn/start` | queue delivery | `threadId, input: [{type: "text", text}], clientUserMessageId` |
| `turn/steer` | steer delivery | needs `expectedTurnId` = the active turn |
| `turn/interrupt` | interrupt | `threadId, turnId`; the turn ends with status `interrupted` |

## Notifications mapped to events

| Codex | Switchyard event |
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
rejected with `-32601`, so the turn never hangs waiting on Switchyard.

## Experimental surface in use

`experimentalApi: true` is required for `availableDecisions`. Nothing else
experimental is relied on.
