# Decisions and departures from the design doc

**The name.** The project was called Switchyard (`swyd`) until just before
v0.1.0. NVIDIA's NeMo Switchyard, a model router that launches Codex and
Claude Code under a `switchyard` command, sits in the same space, and JBoss
SwitchYard is an older integration framework, so it became Switchworks
(`swk`). Nothing had been released, so there is no migration.

**Standard library only.** The build environment could not reach the Go
module proxy, so the module has zero third-party dependencies. That also fits
the lean, static-binary goal. Two design choices changed because of it:

- **Storage: file store instead of SQLite.** `internal/store.Store` is an
  interface. `FileStore` keeps metadata in an atomically rewritten
  `state.json` and each session's events in an append-only JSONL file with
  an in-memory seq index. Event writes are fsynced before the node is
  acknowledged; a write that fails partway is cut back off, and a torn
  final line is truncated on open. Event files are opened and indexed when
  first used, a bounded number at a time. Session records are saved at most
  every 500 ms, so each carries the seq it was projected through, and
  events after it are projected again at startup. Adding a SQLite
  implementation (modernc.org/sqlite, pure Go) later is a drop-in.
- **Node link: NDJSON over mTLS instead of gRPC.** Same semantics (one
  long-lived bidirectional stream per node, node-initiated), with no protobuf
  toolchain. `ProtocolVersion` is in the hello so it can evolve.

**Codex types are hand-written, not generated.** The schema bundle is 5.5 MB
of `oneOf` unions that generators handle poorly. Switchworks declares only the
subset it uses, and a contract test checks every method, notification,
field and enum value against the committed schema. Upgrading Codex means:
regenerate the schema into a new directory, bump `codex.SchemaVersion`, and
fix what the test reports.

**One app-server process per node**, shared by all of that node's Codex
threads. If it dies, the adapter restarts it and resumes every live thread.
See codex-app-server-notes.md for the option of using Codex's own shared daemon instead.

**Delivery semantics.** The default mode, `auto`, puts a message into the
target's running turn when its harness can steer (Codex `turn/steer`), so
it is seen within one tool call rather than hours later, and never piles
up; otherwise it starts a turn. Steering only adds input, so any agent may
steer any session (a session can opt out with `accept_steer: false`), but
interrupting cancels work, so agents interrupt only their own children
unless `allow_steer_any` is set. Messages that arrive while a steer is in
flight go in as one steer. A message queued with `queue` (or by a harness
that cannot steer) waits for the session to be idle, then becomes a new
turn, together with every other message waiting then (one turn for the
backlog, not one each: each turn costs a full-context model call).
People's messages wait ahead of agents'. `steer` goes into the running turn and
falls back to `queue` when there is none. Messages from
people are passed through verbatim; messages from anything else get a
one-line `[switchworks msg=… from=… kind=…]` header.

**Events are numbered by the node, deduplicated by the coordinator.** The
coordinator stores an event only if its seq is above the session's last
stored seq. The node resends everything above the acked seq after any
reconnect. That gives at-least-once delivery and exactly-once storage. Token
deltas are coalesced on the node before they are numbered.

**Antigravity: one long-lived process per session** (see "ACP agents, and
agy" below). Each prompt is a `{"event":"user",...}` line on the stdin of an
`agy --output-format stream-json --input-format stream-json` process, now
that agy documents that schema. The original one-process-per-turn mode
(prompt on stdin, then stdin closed) remains as `agy.per_turn`.

**Enrollment.** `swk coordinator enroll-token <host>` mints a one-time,
expiring token. The token carries the coordinator's API address and the CA's
SHA-256 fingerprint, so `swk node enroll` can trust the coordinator before
it has the CA. The node's certificate CN is its host id; the coordinator
ignores whatever subject the CSR requested.

**Admin access.** `init` makes one admin bearer token, stored hashed in
`coordinator.json` and in plain text in the 0600 `cli.json`. Agents can read
any `cli.json` on their machine, so the owner's other machines get named
API tokens instead (`swk token create`), each with one of three scopes:
`read`, `operate` and `admin`. Still one owner, so tokens limit what a
credential can do, not who it is. Tokens live in the coordinator store as a
SHA-256 hash, like enrollment tokens (they are 256-bit random, so a slow
hash adds nothing), and are deleted when revoked.

**Ended sessions are kept for 90 days.** Every session's record lives in
`state.json`, which is rewritten as sessions change, and each has an event
log, so keeping all of them forever would grow the store and slow every
save. Stopped sessions, and those their node no longer has (gone), are
deleted 90 days after they ended (`session_retention_days`; a negative
value keeps them), counted from `ended_at`, which the store sets by the
coordinator's clock whatever ends a session: a node's clock, or a session
idle for months before its host was removed, cannot shorten it. Running and
errored sessions are kept, and so is any session a running one was spawned
below, since spawn depth is counted up the chain of parents. A node can
still hold a session the coordinator calls gone, and the coordinator adopts
sessions it does not know, so a removed session leaves a tombstone for a
year: its node cannot bring it back or reuse its id. Failed spawns are
forgotten without one, so a spawn that started late is still adopted.
Removal goes in batches of 200, one `state.json` write each. `swk rm`
deletes one sooner; it needs admin, since an operate token is what agents'
machines carry, and history is worth more than the space.

**The coordinator can have an account of its own.** On a machine where
agents also run, a coordinator running as you is readable by them: its CA
key signs node certificates and its admin `cli.json` controls the fleet.
A rootless container under the same account does not help (`podman
unshare` reaches its volume). So on Linux and Windows `swk service install
--system coordinator` makes it a system service under a dedicated
unprivileged account: `switchworks`, declared in a sysusers.d file the
packages ship (so the package manager creates it), or Windows' virtual
account `NT SERVICE\swk-coordinator`, which needs no password and runs only
that service. Root or an administrator sets it up; the coordinator never
runs as either. Its unit is sandboxed (no capabilities, read-only system,
no home directories, system-call filter), which a system unit can do
without the user namespaces a user unit would need. The service never runs
a binary a non-administrator could replace: a `swk` outside root-only
directories is copied to one. `swk --system <command>` runs coordinator
and CLI commands as the account (on Linux, in a child process with only
its user and group; on Windows, from the elevated prompt, whose files in
the coordinator's folder inherit its permissions), never the node or the
MCP server. The node stays a user service: sessions need your sign-ins.
macOS waits for a signed installer package that can create the account.

`operate` includes spawning. A person's spawn is not confined like an
agent's, so an operate token can start an agent with any prompt and any
sandbox on any host. Leaving spawn out would make operate tokens useless
for starting work from a phone or a workstation CLI, which is most of what
they are for, while an agent holding one could already reach every session
through messages. What operate cannot do is change the fleet's membership
or credentials, so a leaked one can be revoked without re-enrolling
anything. `read` is there for machines where even that is too much.

Scopes are enforced in one place: every API route declares its scope in a
table, and the auth middleware looks up the route the mux matched. A route
without a declaration needs admin, and a test walks the table. Cookies and
one-time links carry the scope (and the token id) inside the HMAC-signed
payload, capped at the minting credential's scope, and a cookie is only
honoured while its token still exists.

## Messaging

**The coordinator routes everything.** Agents never talk to each other's
nodes. The router resolves the address, applies the guards, stores the
message, and delivers it through the target's node. One place to enforce
limits, one traffic log, and no node-to-node trust.

**Tools over per-thread MCP, not a config file.** Codex accepts
`config.mcp_servers` on `thread/start` and `thread/resume`, so the node
injects `swk mcp` per session with that session's credentials in its env.
Nothing touches `~/.codex/config.toml`, and a session cannot use another's
identity. `default_tools_approval_mode = "approve"` is required for the
tools to run under the `never` approval policy.

**`swk mcp` is a thin shim.** It speaks MCP on stdio and forwards each call
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
token pasted, so `swk ui-link` mints one-time links (code in the URL
fragment), with the scope of the token that minted them. The cookie is HMAC-signed, `HttpOnly`, `SameSite=Strict`, and
cookie-authenticated writes need a custom header. That is the CSRF defense,
with no token plumbing in the pages.

**Certificates: install the CA or front it with Tailscale.** The private CA
stays the default. `http_listen` plus `tailscale serve` gives a publicly
trusted certificate without Switchworks handling ACME.

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
option kinds, and a second option of a kind, are offered as
`option:<option id>`, so an agent cannot name an option `accept` or
`cancel` and have it chosen in place of the one the name stands for.

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
option. The node merges a credential-free `swk` entry into the
workspace's `.agents/mcp_config.json` (never touching other entries or an
unparsable file) and an `mcp(swk/*)` allow rule into agy's settings. It is
off by default because it changes files outside Switchworks's own
directories.
