# Messaging

Agents talk to each other, and to people, through tools that Switchyard
gives every managed Codex session over MCP. The coordinator routes every
message, so it works the same within one host and across hosts.

## How the tools get into a session

When a node starts or resumes a Codex thread it passes, in `thread/start` /
`thread/resume`, a per-thread MCP server:

```json
{"config": {"mcp_servers": {"swyd": {
  "command": "/usr/local/bin/swyd", "args": ["mcp"],
  "env": {"SWYD_NODE_URL": "http://127.0.0.1:41234", "SWYD_SESSION": "<id>",
          "SWYD_SESSION_PATH": "devbox/lead", "SWYD_TOKEN": "<hmac>"},
  "startup_timeout_sec": 30, "tool_timeout_sec": 1860,
  "default_tools_approval_mode": "approve"}}}}
```

Nothing is written to the user's `config.toml`. Codex starts `swyd mcp` with
only `HOME`, `PATH` and that `env`. `swyd mcp` forwards each tool call to the
node's local API (loopback only), authenticated with
`HMAC(node secret, session id)`, so one session's tools act only as that
session. The node forwards the call to the coordinator over its existing
mTLS link. (This keeps sessions apart through the tools; it is not a
boundary against an agent with a shell: see [Trust model](#trust-model).)

`default_tools_approval_mode = "approve"` matters: without it Codex refuses
MCP calls under the `never` approval policy ("requires approval, but
approval policy is never").

The model sees the tools in the `mcp__swyd` namespace.

## Tools

| Tool | What it does |
| --- | --- |
| `fleet_list` | sessions across the fleet, with status (filter by host or label) |
| `fleet_send` | fire-and-forget message; `mode` queue / steer / interrupt; `broadcast` with an `@selector` |
| `fleet_ask` | ask and wait (default 10 min, max 30) for the answer; returns a ticket on timeout |
| `fleet_reply` | answer an ask, by its `msg` id |
| `fleet_inbox` | recent traffic for this session; with `ticket`, check or wait on an ask |
| `fleet_spawn` | start a helper session (by default on the same host, harness and directory tree); a completion notice comes back when it settles |
| `fleet_status` | a session's status, model and usage; its last message for the caller itself, its parent and its children |
| `fleet_stop` | stop a session you spawned |

## Addresses

- `host/name`: a session path.
- `name`: a session name that is unique among live sessions.
- `parent`: the session that spawned this one.
- `@key=value[,key=value]`: sessions with those labels. `host` and `harness`
  work as keys too. Without `broadcast`, the best match wins (idle and online
  first, then by path).
- `user:<name>`: a person. They read it in the web UI's Inbox or with `swyd inbox`, and answer asks
  there or with `swyd reply`.

## What the receiving agent sees

A routed message becomes a turn (or, with `steer`, part of the running turn)
that starts with a header:

```
[switchyard msg=01M40… from=devbox/lead kind=ask]
(devbox/lead is waiting for an answer: reply with the fleet_reply tool, message_id=01M40….)
what is 6x7?
```

Kinds: `notify`, `ask`, `reply` (with `re=<ask id>`), `done` (completion
notice), `system` (delivery failures and expiry). Plain messages from people
(`swyd send`) are passed through without a header. A line in a relayed
message that looks like a header is prefixed with `(quoted) `, so an agent
cannot pass its text off as a person's.

A spawned session's first prompt arrives the same way: from a person it is
passed through; from an agent's `fleet_spawn` it is a `kind=notify` message
from that agent.

## Delivery

- Every message is stored before delivery is attempted.
- A message to a host that is offline stays **pending** and is delivered
  when the host reconnects (the router retries every 2 s). It expires after
  its TTL (default 24 h), and the sender gets a `system` notice.
- The node deduplicates by message id, so redelivery after a reconnect has
  no effect.
- `steer` falls back to `queue` when the target has no running turn. The
  returned mode says which happened.
- `interrupt` puts the message at the head of the queue and cancels the
  running turn.

## Asks

`fleet_ask` in `swyd mcp` is two calls: it sends the ask with `wait_seconds:
0` and a hold window, gets a ticket at once, then waits for the answer with
`fleet_inbox(ticket)`. The wait is retried if the node's link to the
coordinator drops, so a coordinator restart does not lose the answer.

While the hold window is open, a reply that arrives before the asker starts
waiting is **held** for it rather than also being delivered as a new turn.
After the window, an unanswered ask's reply arrives as a `kind=reply` turn.

A person can ask an agent (`swyd ask <address> <question>`) and an agent
can ask a person (`fleet_ask` to `user:<name>`).

## Completion notices

A node emits `session.settled` when a session's last turn has completed and
its queue is empty. If that session was spawned by another, the parent gets
a `kind=done` message with the child's last reply. It is skipped when the
child's work was an ask from the parent that has been answered: the answer
already said it.

## Guards

Set in the coordinator's `coordinator.json` under `"guards"`:

| Field | Default | Effect |
| --- | --- | --- |
| `max_hops` | 8 | longest chain of message-triggered turns; a send that would exceed it is refused |
| `send_per_minute` | 30 | per sender → target pair (all `user:` recipients count as one target) |
| `out_per_minute` | 60 | per agent, all its messages together |
| `ask_per_minute` | 10 | per sender → target pair |
| `max_spawn_depth` | 2 | spawned sessions may spawn, up to this depth |
| `spawn_per_minute` | 3 | spawns per agent |
| `spawn_hosts` | the agent's own host | other hosts agents may spawn on; `["*"]` for any |
| `spawn_harnesses` | the agent's own harness | other harnesses agents may spawn; `["*"]` for any |
| `spawn_cwd_roots` | the agent's own cwd | other directories agents may spawn in (and below); `["*"]` for any |
| `max_daily_tokens` | off | fleet-wide; `fleet_spawn` is refused once reached (UTC day) |
| `allow_stop_any` | false | let agents stop sessions they did not spawn |
| `allow_steer_any` | false | let agents steer or interrupt sessions they did not spawn |
| `allow_read_any` | false | let `fleet_status` show any session's last message to agents |
| `max_broadcast` | 8 | recipients of one broadcast from an agent |
| `allow_unsandboxed_spawn` | false | let a sandboxed agent spawn a harness that does not enforce sandboxes (ACP agents such as goose) |
| `default_ttl_seconds` | 86400 | how long an undelivered message waits |

People are not rate limited. Hops count from the message that started the
sender's current work: a person's message is hop 0, the agent's message to
another agent is hop 1, and so on. A spawn counts as a hop too, so a loop of
spawns and completion notices ends at `max_hops`.

A session an agent spawns always inherits the agent's approval policy and
sandbox setting, but only Codex and agy enforce a sandbox: ACP agents such
as goose run with the user's full access. So a sandboxed agent may not
spawn one (even where `spawn_harnesses` allows it) unless
`allow_unsandboxed_spawn` is set. The child's working directory is checked
on the target host with symlinks (and Windows junctions) resolved. Agents cannot set the label keys `host`, `harness`,
`parent` or any key starting with `owner.`, so routes such as
`@owner.role=reviewer` stay under your control. (Plain labels such as `@role=reviewer`, and
the paths of stopped sessions, can still be taken by an agent's children.)
An agent's spawn is refused on a node too old to enforce the directory
check, and a parent whose policies are unknown gives its child the
strictest ones (`untrusted`, `read-only`).

## Trust model

- **People** hold the admin token (`cli.json`) or a web sign-in. They can do
  anything.
- **Nodes** hold a certificate for one host. A node can only report on and
  run sessions of its own host. `swyd hosts rm <host>` revokes a host's
  certificate, and enrolling a host again replaces it.
- **Agents** act through the fleet tools, within the guards above.

The guards are not a sandbox. An agent that can run commands as your user
on its workstation can read that node's key, other sessions' tokens and
any `cli.json` there. Harness sandboxes do not prevent this: Codex's
`workspace-write` and `read-only` modes restrict writes and network access,
not reads. A sandboxed agent cannot use what it reads directly (no network),
but it could pass it on through the fleet tools to an unsandboxed session.
Keeping these secrets from agents needs an OS boundary, such as running the
node and its agents as a separate user. In particular:

- Do not put `cli.json` (the admin token) on a machine where agents run:
  an agent that reads it, sandboxed or not, can get it to one that can use
  it, and the admin token controls the fleet.
- Prefer sandboxed sessions, and no unsandboxed peers on the same fleet,
  for agents that read untrusted input (web pages, issues, other people's
  code), since messages and spawns are how a manipulated agent would reach
  the rest of the fleet.
- If a workstation may be compromised, `swyd hosts rm` it.

## CLI

```sh
swyd msg buildbox/helper "the build is green"        # by address
swyd msg --broadcast @role=reviewer "please look at PR 42"
swyd ask lead "are you done with the parser?"    # waits for fleet_reply
swyd inbox                                       # messages and open questions for people
swyd reply <ask-id> "yes, go ahead"
swyd messages [session]                          # traffic log
swyd usage                                       # tokens by host, model, day
```

## HTTP API

| Method | Path | |
| --- | --- | --- |
| POST | `/v1/messages` | `{to, text, kind: notify|ask|broadcast, mode, ttl_seconds, wait_seconds, from}` |
| GET | `/v1/messages?session=&inbox=1&limit=` | newest first |
| GET | `/v1/messages/{id}` | |
| POST | `/v1/messages/{id}/reply` | `{text, from}`: a person answers an ask |
| GET | `/v1/usage` | daily totals, by host, by model |
| POST | `/v1/sessions/{id}/messages` | as before; now routed (202 with `state`) |
