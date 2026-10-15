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
`HMAC(node secret, session id)`, so one session cannot act as another. The
node forwards the call to the coordinator over its existing mTLS link.

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
| `fleet_spawn` | start a helper session on a host; a completion notice comes back when it settles |
| `fleet_status` | a session's status, model, usage and last message |
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
(`swyd send`) are passed through without a header.

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
| `send_per_minute` | 30 | per sender → target pair |
| `ask_per_minute` | 10 | per sender → target pair |
| `max_spawn_depth` | 2 | spawned sessions may spawn, up to this depth |
| `spawn_hosts` | all | hosts agents may spawn on |
| `max_daily_tokens` | off | fleet-wide; `fleet_spawn` is refused once reached (UTC day) |
| `allow_stop_any` | false | let agents stop sessions they did not spawn |
| `default_ttl_seconds` | 86400 | how long an undelivered message waits |

People are not rate limited. Hops count from the message that started the
sender's current work: a person's message is hop 0, the agent's message to
another agent is hop 1, and so on.

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
