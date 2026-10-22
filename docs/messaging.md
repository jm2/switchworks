# Messaging

Agents talk to each other, and to people, through tools that Switchworks
gives every managed Codex session over MCP. The coordinator routes every
message, so it works the same within one host and across hosts.

## How the tools get into a session

When a node starts or resumes a Codex thread it passes, in `thread/start` /
`thread/resume`, a per-thread MCP server:

```json
{"config": {"mcp_servers": {"swk": {
  "command": "/usr/local/bin/swk", "args": ["mcp"],
  "env": {"SWK_NODE_URL": "http://127.0.0.1:41234", "SWK_SESSION": "<id>",
          "SWK_SESSION_PATH": "devbox/lead", "SWK_TOKEN": "<hmac>"},
  "startup_timeout_sec": 30, "tool_timeout_sec": 1860,
  "default_tools_approval_mode": "approve"}}}}
```

Nothing is written to the user's `config.toml`. Codex starts `swk mcp` with
only `HOME`, `PATH` and that `env`. `swk mcp` forwards each tool call to the
node's local API (loopback only), authenticated with
`HMAC(node secret, session id)`, so one session's tools act only as that
session. The node forwards the call to the coordinator over its existing
mTLS link. (This keeps sessions apart through the tools; it is not a
boundary against an agent with a shell: see [Trust model](#trust-model).)

`default_tools_approval_mode = "approve"` matters: without it Codex refuses
MCP calls under the `never` approval policy ("requires approval, but
approval policy is never").

The model sees the tools in the `mcp__swk` namespace.

## Tools

| Tool | What it does |
| --- | --- |
| `fleet_list` | sessions across the fleet, with status and queue length (filter by host or label) |
| `fleet_send` | fire-and-forget message; `mode` auto (default) / queue / steer / interrupt (see [Delivery](#delivery)); `broadcast` with an `@selector`; `images` (see [Images](#images)) |
| `fleet_ask` | ask and wait (default 10 min, max 30) for the answer; returns a ticket on timeout; `images` |
| `fleet_reply` | answer an ask, by its `msg` id |
| `fleet_inbox` | recent traffic for this session; with `ticket`, check or wait on an ask |
| `fleet_spawn` | start a helper session (by default on the same host, harness and directory tree); a completion notice comes back when it settles; `images` go with the prompt |
| `fleet_status` | a session's status, model and usage; its last message for the caller itself, its parent and its children |
| `fleet_stop` | stop a session you spawned |

## Addresses

- `host/name`: a session path.
- `name`: a session name that is unique among live sessions.
- `parent`: the session that spawned this one.
- `@key=value[,key=value]`: sessions with those labels. `host` and `harness`
  work as keys too. Without `broadcast`, the best match wins (idle and online
  first, then by path).
- `user:<name>`: a person. They read it in the web UI's Inbox or with `swk inbox`, and answer asks
  there or with `swk reply`.

## What the receiving agent sees

A routed message becomes part of the target's running turn (by default,
when its harness can steer), or a new turn, and starts with a header:

```
[switchworks msg=01M40… from=devbox/lead kind=ask]
(devbox/lead is waiting for an answer: reply with the fleet_reply tool, message_id=01M40….)
what is 6x7?
```

Messages that waited in the queue go in together, as one turn: each keeps
its own header, in the order they are delivered (see [Delivery](#delivery)),
separated by a blank line.

Kinds: `notify`, `ask`, `reply` (with `re=<ask id>`), `done` (completion
notice), `system` (delivery failures and expiry). A line in a relayed
message that looks like a header is prefixed with `(quoted) `, so no
sender can put a header of its own in its text.

Messages from people name the credential they were sent with, which the
coordinator takes from the request (the `from=user:<name>` part is only
the name the client gave):

```
[switchworks msg=01M41… from=user:cli via=token:devbox kind=notify]
run the tests again
```

Only a plain message (`kind=notify`) sent with the admin token or an
**owner** token (`swk token create --owner`, `swk token owner`), or from a
browser one of those signed in, is passed through without a header. So an
agent can take a message without a header as its owner's, as long as no
owner token is readable on a host where agents run: an agent that reads a
token can send with it. Nodes from before 0.1.2 pass every plain message
from a person through without a header; a node from 0.1.2 under an older
coordinator puts a header (without `via`) on all of them.

A spawned session's first prompt arrives the same way: from a person it
follows the same rule; from an agent's `fleet_spawn` it is a `kind=notify`
message from that agent.

A message with images says so under its header, and the images are part
of the same turn's input:

```
[switchworks msg=01M41… from=devbox/game kind=notify]
(1 image attached, in this turn's input: level1.png (image/png, 412 KiB).)
does level 1 render?
```

## Images

Messages (`fleet_send`, `fleet_ask`, `swk send`/`msg`/`ask`, the web UI)
and first prompts (`fleet_spawn`, `swk spawn`, the web UI) can carry
images, for example a game agent's screenshots for a reviewer agent on
another host.

**Limits.** PNG, JPEG, WebP or GIF only, at most 4 images per message,
5 MiB each and 10 MiB in all. The type is sniffed from the bytes (magic
numbers); a declared type that disagrees is refused. Each check runs on
the sender's node (or CLI or browser), again on the coordinator, and again
on the receiving node. As base64, the largest message is about 13.4 MiB,
well under the 32 MiB limit of a node connection's line. The API accepts
bodies up to 16 MiB on the three endpoints that take images and 4 MiB
elsewhere.

**Which harnesses.** Each node reports which of its harnesses take images,
and each session reports whether it does, so the coordinator refuses
undeliverable images before storing the message:

| Harness | Images | How |
| --- | --- | --- |
| Codex | yes | `localImage` inputs: the node writes each image to a private file and app-server reads it |
| Goose and other ACP agents | if the agent advertises `promptCapabilities.image` (goose 1.54 does) | ACP `image` content blocks after the text |
| agy | no | its headless stream-json input documents text blocks only |

Whether the *model* sees the image is a separate question: a model without
vision, or a provider endpoint that drops images, gets the text only. Use
`swk dev vision-probe` ([harnesses.md](harnesses.md#images-and-the-vision-probe)).

**Agents attach files by path.** The `images` argument is a list of file
paths on the sender's host, relative to the session's working directory
or absolute. `swk mcp` runs as your user, outside any sandbox the agent
has, so if it read whatever path an agent named, a sandboxed agent could
send itself (or another agent) files it cannot read: the node's key,
other sessions' tokens, `cli.json`. So the sender's node reads the file
itself, and only:

- inside the session's working directory, as the node recorded it at
  spawn (not anything the agent can change); a session confined to a
  directory root (spawned by an agent) is checked against that root first;
- reached without any symlink or Windows junction below the working
  directory: the node opens the path one directory at a time, each through
  the one before it, and checks that each is the directory it looked at,
  so a component swapped for a symlink mid-read is refused;
- a regular file: not a symlink (even to a file inside), device, pipe or
  directory, and with no other hard links (on Unix), since another name
  for it could be anywhere;
- never under Switchworks's own directories (the node's directory, the
  config directory with `cli.json`), even when the working directory
  contains them (a session working in your home directory); directories
  are compared by identity, so another spelling of the name (case on
  macOS) does not get past it;
- an allowed image type by its bytes, within the size limits.

Agents cannot pass image data directly: `swk mcp` and the node's local API
refuse an `attachments` argument (in any letter case). Paths never reach
the coordinator.

**Delivery and storage.** The coordinator keeps message records (and every
event) with the images' metadata only: name, type, size and SHA-256. It
keeps the bytes in `<coordinator dir>/attachments/<sha256>` (0700; files
0600; names are digests, never a sender's file name):

- while a message carrying them is pending, so it can still be delivered
  when an offline host returns; these are never evicted;
- after delivery, for 7 days, so the web UI can show them in transcripts,
  the inbox and the traffic list; the oldest go first while the store
  holds more than 512 MiB.

New images are refused while the store holds 1 GiB. Every guard runs
before an agent's images are stored, and one agent may send at most
20 MiB of images a minute, but a busy fleet can still push delivered
images out of the store sooner than 7 days (the messages keep their
metadata). Keeping bytes in the message records instead would
put megabytes into `messages.jsonl` on every state change and into the
coordinator's memory, which holds all message records.

On the receiving node a queued message keeps only metadata in
`state.json`; its images wait in `<node dir>/attachments/<session>/` until
the message is delivered. Codex's image files go in
`<node dir>/scratch/<session>/` and are removed when the turn completes,
when the node restarts, or when the session stops. Events
(`message.queued`, `message.delivered`, the turn's `userMessage` item)
carry metadata only.

## Delivery

- Every message is stored before delivery is attempted.
- A message to a host that is offline stays **pending** and is delivered
  when the host reconnects (the router retries every 2 s). It expires after
  its TTL (default 24 h), and the sender gets a `system` notice.
- The node deduplicates by message id, so redelivery after a reconnect has
  no effect.
- Delivery modes:

  | Mode | Target running a turn | Target idle |
  | --- | --- | --- |
  | `auto` (default for `fleet_send`, `swk send`/`msg`, the web UI and the API) | into that turn (Codex `turn/steer`), seen at its next tool call | starts a turn |
  | `queue` | waits for the turn to end, then starts the next one | starts a turn |
  | `steer` | into that turn, even if the session does not accept steering (for people, and an agent's own children) | starts a turn |
  | `interrupt` | goes to the head of the queue and cancels the turn | starts a turn |

  Asks, replies, completion notices and Switchworks's notices are queued.
  The returned mode says what happened (`steer` or `queue`). Harnesses that
  cannot steer (agy, ACP agents) queue instead, and a session spawned with
  `accept_steer: false` (`swk spawn --no-steer`) gets `auto` messages, and
  other agents' steers, queued: for a long build that must not be
  distracted. Nodes before 0.1.5 take `auto` as `queue`.
- Messages that arrive while a steer is being delivered go in together, as
  one steer input, each with its own header.
- `interrupt` puts the message at the head of the queue and cancels the
  running turn.
- When a session goes idle, the messages in its queue are delivered
  together as **one** turn, each with its own header, rather than one turn
  each (every turn resends the session's whole context to the model). One
  turn takes at most 50 messages, 256 KiB of text and one message's image
  limits; the rest go in the next. Each message is still reported
  delivered on its own (`message.delivered`) and keeps its id.
- The queue puts people first: the owner's plain messages, then other
  people's, then agents' and Switchworks's own, each in the order they
  came. `interrupt` goes ahead of its own group, never ahead of the
  owner's messages. In a turn, the owner's plain messages (which have no
  header) come first, so their text never follows another sender's.
- Each node reports how many messages wait in each session's queue
  (`session.queue`); `fleet_list`, `fleet_status`, `GET /v1/sessions` and
  the web UI show it. Older nodes do not report it.
- A queued message can be sent now or cancelled, by its sender (the same
  credential) or an owner (the admin token, an owner token, or a browser
  one of those signed in): `POST /v1/messages/{id}/promote` with `mode`
  `steer` (into the running turn; with none running it goes next, ahead of
  the queue) or `interrupt`, and `POST /v1/messages/{id}/cancel`, after
  which the node never delivers it. The node records `message.promoted`
  and `message.cancelled`; a cancelled message's state is `cancelled`, an
  agent that sent it gets a `system` notice, and a cancelled ask fails.
  `GET /v1/sessions/{id}/queue` lists a session's queue in delivery order.
  These need a node from 0.1.5 (an older one answers 409).
- A queued message is delivered when its session goes idle. If the harness
  still says it is busy (Codex reports the thread idle a moment before the
  turn ends), the node tries again once the turn ends, or after 1, 5 and
  then every 30 s. Every 15 s it also looks for sessions that have been
  idle with queued messages for 30 s, logs a warning and tries again.

## Asks

`fleet_ask` in `swk mcp` is two calls: it sends the ask with `wait_seconds:
0` and a hold window, gets a ticket at once, then waits for the answer with
`fleet_inbox(ticket)`. The wait is retried if the node's link to the
coordinator drops, so a coordinator restart does not lose the answer.

While the hold window is open, a reply that arrives before the asker starts
waiting is **held** for it rather than also being delivered as a new turn.
After the window, an unanswered ask's reply arrives as a `kind=reply` turn.

An ask fails, and the asker gets a `system` notice (and a waiting
`fleet_ask` or ticket says so at once), when its target's harness does not
take it, or when the target stops or goes away before answering.
Unanswered asks are kept 14 days, like finished messages.

A person can ask an agent (`swk ask <address> <question>`) and an agent
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
| `spawn_cwd_roots` | the agent's own cwd (and below, for a read-only agent) | other directories agents may spawn in (and below); `["*"]` for any. On another host only these count, so a cross-host spawn needs them |
| `max_daily_tokens` | off | fleet-wide; `fleet_spawn` is refused once reached (UTC day) |
| `allow_stop_any` | false | let agents stop sessions they did not spawn |
| `allow_steer_any` | false | let agents interrupt sessions they did not spawn, and steer ones that do not accept steering (any agent may steer the rest: steering only adds input to a turn) |
| `allow_read_any` | false | let `fleet_status` show any session's last message to agents |
| `max_broadcast` | 8 | recipients of one broadcast from an agent |
| `allow_unsandboxed_spawn` | false | let a sandboxed agent spawn a harness that does not enforce sandboxes (ACP agents such as goose), or a read-only one spawn agy |
| `allow_unapproved_spawn` | false | let an agent that asks for approval spawn agy, which never asks |
| `default_ttl_seconds` | 86400 | how long an undelivered message waits; also the longest `ttl_seconds` a sender may ask for |

The coordinator keeps at most 256 MiB of messages; past that the oldest
delivered (answered or not), failed or expired ones are dropped first.

People are not rate limited. Hops count from the message that started the
sender's current work: a person's message is hop 0, the agent's message to
another agent is hop 1, and so on. A spawn counts as a hop too, so a loop of
spawns and completion notices ends at `max_hops`.

A session an agent spawns always inherits the agent's approval policy and
sandbox setting, but only Codex and agy enforce a sandbox: ACP agents such
as goose run with the user's full access. So a sandboxed agent may not
spawn one (even where `spawn_harnesses` allows it) unless
`allow_unsandboxed_spawn` is set. agy's sandbox restricts only its
terminal, not its edit tools, so a read-only agent may not spawn agy
either, unless `allow_unsandboxed_spawn` is set.

The child's working directory is checked on the target host with symlinks
(and Windows junctions) resolved, and recorded resolved: the node checks
before each turn that it still leads to the same directory, and the
child's own children are confined to that real path. An agent that can
write in its working directory (Codex `workspace-write`, or agy in any
sandbox) could swap a directory below it for a symlink once its child has
been checked there, so it may spawn only in its working directory itself,
or in a `spawn_cwd_roots` directory outside it (not below it, even with
`["*"]`). Keep those roots out of places sandboxed agents can write, such
as `/tmp`. A read-only agent may spawn below its working directory.

Agents cannot set the label keys `host`, `harness`,
`parent` or any key starting with `owner.`, so routes such as
`@owner.role=reviewer` stay under your control. (Plain labels such as `@role=reviewer`, and
the paths of stopped sessions, can still be taken by an agent's children.)
An agent's spawn is refused on a node too old to enforce these directory
checks, and a parent whose policies are unknown gives its child the
strictest ones (`untrusted`, `read-only`). Policies a node reported for its
own default (rather than ones set at spawn) do not carry to another host;
the child gets the strictest ones there too. An ACP child of an agent that
asks for approval must get a matching mode from its agent, or it does not
start.

## Trust model

- **People** hold the admin token, a scoped API token (`swk token
  create`, in a `cli.json`) or a web sign-in. The admin token can do
  anything; an API token, and a browser it signed in, only what its scope
  allows: `read` looks, `operate` also sends, asks, replies, approves,
  interrupts, stops, spawns and tightens approval policies (loosening one
  needs the admin token or an owner token), `admin` also
  enrolls and removes hosts and manages tokens. A person's spawn is not
  confined by the guards below. Messages carry the credential they were
  sent with; only the admin token's and owner tokens' plain messages reach
  agents without a header (see
  [What the receiving agent sees](#what-the-receiving-agent-sees)).
- **Nodes** hold a certificate for one host. A node can only report on and
  run sessions of its own host. `swk hosts rm <host>` revokes a host's
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

- Do not put the admin token on a machine where agents run: an agent that
  reads it, sandboxed or not, can get it to one that can use it, and the
  admin token controls the fleet. The same goes for owner tokens: an agent
  holding one can send messages that reach other agents as yours. Give
  those machines at most a `read` token (`swk token create devbox --scope
  read --cli-config`). An `operate` token there lets an agent do what the
  token allows: approve its own prompts, spawn unconfined sessions and
  message others (with a header naming the token), but not loosen an
  approval policy.
  No token there can enroll a host, remove hosts or mint tokens, and `swk
  token rm devbox` cuts it off without touching the other machines.
- The coordinator's directory holds the CA key and the admin `cli.json`:
  do not run agents as the coordinator's user on its machine. Running the
  coordinator in a rootless Podman container under that same user does not
  change this: the files are no longer plainly readable, but `podman
  unshare` and `podman exec` reach them as that user. A container (or a
  plain service) under a separate account, with no agents, does.
- Prefer sandboxed sessions, and no unsandboxed peers on the same fleet,
  for agents that read untrusted input (web pages, issues, other people's
  code), since messages and spawns are how a manipulated agent would reach
  the rest of the fleet.
- If a workstation may be compromised, `swk hosts rm` it.

The fleet tools themselves do not widen what an agent can read: the files
an agent attaches as images are confined to its working directory and
never come from Switchworks's own directories ([Images](#images)). Images
an agent receives are model input like any message text, and a crafted
image can carry instructions the same way text can.

## CLI

```sh
swk msg buildbox/helper "the build is green"         # by address
swk msg --broadcast @role=reviewer "please look at PR 42"
swk msg --image shot.png --image diff.png reviewer "these two frames"
swk ask lead "are you done with the parser?"     # waits for fleet_reply
swk inbox                                        # messages and open questions for people
swk reply <ask-id> "yes, go ahead"
swk messages [session]                           # traffic log
swk usage                                        # tokens by host, model, day
```

## HTTP API

| Method | Path | |
| --- | --- | --- |
| POST | `/v1/messages` | `{to, text, kind: notify\|ask\|broadcast, mode: auto (default)\|queue\|steer\|interrupt, ttl_seconds, wait_seconds, from, attachments}` |
| GET | `/v1/messages?session=&inbox=1&limit=` | newest first |
| GET | `/v1/messages/{id}` | |
| POST | `/v1/messages/{id}/reply` | `{text, from}`: a person answers an ask |
| GET | `/v1/usage` | daily totals, by host, by model |
| POST | `/v1/sessions/{id}/messages` | `{text, mode (default auto), from, attachments}`; routed (202 with `state` and `mode`) |
| POST | `/v1/sessions` | a spawn; `attachments` go with `prompt`; `accept_steer: false` keeps `auto` messages out of its running turns |
| GET | `/v1/attachments/{sha256}` | a stored image: `{mime, size, sha256, data}` (base64) |
| GET | `/v1/sessions/{id}/queue` | the session's queued messages in delivery order: `{session, messages: [{id, from, kind, owner, images, position, text, via, created_at}]}`; `position` counts those ahead |
| POST | `/v1/messages/{id}/promote` | `{mode: steer\|interrupt}` (default steer): deliver a queued message now; the sender or an owner |
| POST | `/v1/messages/{id}/cancel` | take a queued (or still pending) message out, so it is never delivered; the sender or an owner |

Reading needs a `read` token; sending, asking, replying, promoting and
cancelling need `operate`.
API tokens are managed with `POST /v1/tokens` (`{name, scope, ttl_seconds}`;
the secret comes back once), `GET /v1/tokens` and `DELETE
/v1/tokens/{id-or-name}`, all admin only. A request beyond a token's scope
gets 403 naming the scope it needs. Reading a stored image
needs `read`.

`attachments` is a list of `{name, mime, data}` with `data` in base64;
`mime` may be left out (it is sniffed). Messages returned by the API carry
`attachments` without `data`.
