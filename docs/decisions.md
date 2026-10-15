# Decisions and departures from the design doc

**Standard library only.** The build environment could not reach the Go
module proxy, so the module has zero third-party dependencies. That also fits
the lean, static-binary goal. Two design choices changed because of it:

- **Storage: file store instead of SQLite.** `internal/store.Store` is an
  interface. `FileStore` keeps metadata in an atomically rewritten
  `state.json` and each session's events in an append-only JSONL file with
  an in-memory seq index. Event writes are fsynced before the node is
  acknowledged. A torn final line is truncated on open. Adding a SQLite
  implementation (modernc.org/sqlite, pure Go) later is a drop-in.
- **Node link: NDJSON over mTLS instead of gRPC.** Same semantics (one
  long-lived bidirectional stream per node, node-initiated), with no protobuf
  toolchain. `ProtocolVersion` is in the hello so it can evolve.

**Codex types are hand-written, not generated.** The schema bundle is 5.5 MB
of `oneOf` unions that generators handle poorly. Switchyard declares only the
subset it uses, and a contract test checks every method, notification,
field and enum value against the committed schema. Upgrading Codex means:
regenerate the schema into a new directory, bump `codex.SchemaVersion`, and
fix what the test reports.

**One app-server process per node**, shared by all of that node's Codex
threads. If it dies, the adapter restarts it and resumes every live thread.
See codex-app-server-notes.md for the option of using Codex's own shared daemon instead.

**Delivery semantics.** A message queued with `queue` waits for the session
to be idle, then becomes a new turn. `steer` goes into the running turn and
falls back to `queue` when there is none. Messages from
people are passed through verbatim; messages from anything else get a
one-line `[switchyard msg=… from=… kind=…]` header.

**Events are numbered by the node, deduplicated by the coordinator.** The
coordinator stores an event only if its seq is above the session's last
stored seq. The node resends everything above the acked seq after any
reconnect. That gives at-least-once delivery and exactly-once storage. Token
deltas are coalesced on the node before they are numbered.

**Antigravity runs one process per turn.** This uses only documented flags
(`--output-format stream-json`, `--conversation`, `--model`,
`--dangerously-skip-permissions`) with the prompt on stdin. The long-lived
`--input-format stream-json` mode is faster, but its stdin schema is
undocumented, so it waits until a real capture shows the shape.

**Enrollment.** `swyd coordinator enroll-token <host>` mints a one-time,
expiring token. The token carries the coordinator's API address and the CA's
SHA-256 fingerprint, so `swyd node enroll` can trust the coordinator before
it has the CA. The node's certificate CN is its host id; the coordinator
ignores whatever subject the CSR requested.

**Admin access.** The CLI uses one bearer token, stored hashed in
`coordinator.json` and in plain text in the 0600 `cli.json`. It is a single
owner on a LAN plus Tailscale. Per-user tokens are not needed yet.

## Messaging

**The coordinator routes everything.** Agents never talk to each other's
nodes. The router resolves the address, applies the guards, stores the
message, and delivers it through the target's node. One place to enforce
limits, one traffic log, and no node-to-node trust.

**Tools over per-thread MCP, not a config file.** Codex accepts
`config.mcp_servers` on `thread/start` and `thread/resume`, so the node
injects `swyd mcp` per session with that session's credentials in its env.
Nothing touches `~/.codex/config.toml`, and a session cannot use another's
identity. `default_tools_approval_mode = "approve"` is required for the
tools to run under the `never` approval policy.

**`swyd mcp` is a thin shim.** It speaks MCP on stdio and forwards each call
to the node's loopback API, which forwards it to the coordinator on the
existing mTLS link. No new listener is exposed off the host, and the shim
holds no state.

**Tool results are text only.** Codex 0.160 shows the model a tool's
`structuredContent` instead of its text content when both are present. The
text is written for the model ("Answer from buildbox/helper: …"), so that is
what is sent.

**Offline targets wait instead of failing.** Agents work asynchronously,
so a message to an offline host is stored as pending, delivered when the
host returns, and expired after a TTL with a notice to the sender. Steering still requires a live session.

**Asks are two-phase in the shim.** One long HTTP call would lose the
answer if the node's coordinator link dropped mid-wait. The shim sends the
ask with no wait and a hold window, then waits on the ticket with
`fleet_inbox`, retrying transient failures. A reply that arrives during the
hold window is held for the waiting asker instead of also becoming a turn.

**Completion notices come from the node's settled signal.** A session is
settled when its last turn completed and its queue is empty. Codex reports
idle before `turn/completed`, so settled also requires no active turn. The
notice to the parent is skipped when the child was only answering the
parent's ask.

**Guards are coarse on purpose.** Hop limit, per-pair token buckets, spawn
depth, spawn host allowlist and a fleet-wide daily token budget. They stop
runaway loops and spend; they are not an authorization system. People are
not rate limited.

## Web UI

**Served by the coordinator, embedded in the binary.** No separate web
server, no npm. Vanilla JavaScript over the same JSON API and SSE streams
the CLI uses. That keeps one source of truth for behavior and makes the UI a
thin client that is cheap to change.

**A fleet stream instead of polling.** `GET /v1/stream` sends a snapshot
(hosts, sessions, recent messages), then session, host and message updates.
Session updates carry each session's last agent message and open approval
prompts. The coordinator keeps that activity in memory and rebuilds it from
the tail of each log at startup, so it is not a second source of state.

**Cookie sign-in next to the bearer token.** Phones should not need the
token pasted, so `swyd ui-link` mints one-time links (code in the URL
fragment). The cookie is HMAC-signed, `HttpOnly`, `SameSite=Strict`, and
cookie-authenticated writes need a custom header. That is the CSRF defense,
with no token plumbing in the pages.

**Certificates: install the CA or front it with Tailscale.** The private CA
stays the default. `http_listen` plus `tailscale serve` gives a publicly
trusted certificate without Switchyard handling ACME.

**Approvals live by the composer.** On a phone the action you need is at
the bottom, within reach, and the transcript only notes that an approval
was requested and how it was answered.

## ACP agents, and agy

**ACP before observed sessions.** Goose over ACP reaches DeepSeek, GLM and
MiMo through their Anthropic-compatible endpoints, which removes the need
for a Responses-to-Chat proxy in front of Codex. The ACP client is
hand-written on `internal/jsonrpc` (now with an optional `"jsonrpc":"2.0"`
field and a split send/wait call), keeping the module dependency-free.

**One agent process per ACP session.** Codex shares one app-server per
node; ACP sessions each get their own process. That isolates crashes and
stuck turns, lets per-agent environment (provider keys) differ by harness
name, and makes Stop and restart simple. The cost is memory per session.

**Normalize tool calls into Codex's item shapes.** ACP tool calls become
`commandExecution`, `fileChange` (with a unified diff computed from the
old and new text), `mcpToolCall`, or a generic `toolCall`. The UI and CLI
render the first three without changes; only `toolCall`, `reasoning` and
`plan` are new.

**Codex's decision vocabulary for ACP approvals.** `allow_once` /
`allow_always` / `reject_once` / `reject_always` map to `accept` /
`acceptForSession` / `decline` / `declineAlways`, plus `cancel`. Unknown
option kinds are offered under their raw option id.

**Fleet tools are approved automatically for ACP agents too.** Goose in
`approve` mode asks before every tool; waiting on a person for
`fleet_reply` would stall agent-to-agent traffic.

**The prompt is written before Deliver returns.** An Interrupt right after
a Deliver must reach the agent after the prompt, or the agent ignores the
cancel. The same holds for agy, whose prompt line is written synchronously.

**Session/load replay is discarded.** The coordinator already has the
history; replayed updates arrive before the load response and are dropped.

**agy: long-lived stream-json process.** `--input-format stream-json` is
now documented, so each session keeps one process; `per_turn` keeps the
single-shot mode. Usage is cumulative per process, so the baseline resets
when a process starts.

**agy fleet tools are opt-in and write files.** agy has no per-run MCP
option. The node merges a credential-free `swyd` entry into the
workspace's `.agents/mcp_config.json` (never touching other entries or an
unparsable file) and an `mcp(swyd/*)` allow rule into agy's settings. It is
off by default because it changes files outside Switchyard's own
directories.
