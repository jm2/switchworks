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
| `fleet_send` | fire-and-forget message to one address or a list of them (see [Several recipients](#several-recipients)); `mode` auto (default) / queue / steer / interrupt (see [Delivery](#delivery)); `broadcast` with an `@selector`; `supersedes` and `ttl_seconds` (see [Superseding](#superseding) and [Delivery](#delivery)); `quiet` (see [Quiet messages](#quiet-messages)); `images` (see [Images](#images)); `files` (see [Files](#files)) |
| `fleet_ask` | ask and wait (default 10 min, max 30) for the answer; returns a ticket on timeout; `images`; `files` |
| `fleet_reply` | answer an ask, by its `msg` id |
| `fleet_inbox` | messages this session has not seen yet (see [Inbox](#inbox)); `all` for recent traffic; with `ticket`, check or wait on an ask |
| `fleet_spawn` | start a helper session (by default on the same host, harness and directory tree); a completion notice comes back when it settles; `images` go with the prompt |
| `fleet_status` | a session's status, model and usage; its last message for the caller itself, its parent and its children |
| `fleet_stop` | stop a session you spawned |
| `fleet_lock` | take a fleet-wide lease on a shared resource (`name`, `ttl_seconds` default 1800 and at most 21600, `wait_seconds` 0 to 1800, `note`): `held` with a `lease_id` and expiry, or `busy` with the holder, since when, its note and expiry (see [Locks](#locks)) |
| `fleet_unlock` | release a lock you hold; the next waiter gets it |
| `fleet_locks` | the locks held now, with holder, note, expiry and waiters (`prefix` to filter) |
| `fleet_remind` | `{in_seconds (60–86400), text, id?}`: a `system` message with `text` comes back to this session after the delay, starting a turn if it is idle; a new one with a pending one's `id` replaces it; `{cancel: id}` cancels one (see [Reminders](#reminders)) |
| `workspace_git` | git on the session's own repository, run by its node outside the sandbox (not through the coordinator): `status`, `sync` (fetch, then rebase or fast-forward onto the upstream, carrying uncommitted work), `commit`, `push` (never main, master or the remote's default branch) and `branch_delete`; see [Git for agents](harnesses.md#git-for-agents-workspace_git) |

## Addresses

- `host/name`: a session path.
- `name`: a session name that is unique among live sessions.
- `parent`: the session that spawned this one.
- `@key=value[,key=value]`: sessions with those labels. `host` and `harness`
  work as keys too. Without `broadcast`, the best match wins (idle and online
  first, then by path).
- `user:<name>`: a person. They read it in the web UI's Inbox or with `swk inbox`, and answer asks
  there or with `swk reply`.

### Several recipients

`fleet_send`'s `to` (and `to` in `POST /v1/messages`, and repeated `--to`
in `swk msg`) can be a list of up to 64 addresses of any of these forms,
so an agent tells four peers the same thing in one tool call rather than
four. Each recipient gets its own message record and delivery state; the
records share a `group` id (as a broadcast's do), and a session named by
two addresses gets one message. The result lists each recipient's message
id and state, or `state: "refused"` with the reason for an address that
cannot take it (unknown, the sender itself, a harness that takes no
images): the others still go. If none can, the call fails and names each.

Limits count every recipient: `max_broadcast` caps the sessions one
agent's send reaches, and each recipient takes its own token from the
`send_per_minute` and `out_per_minute` buckets. A limit refuses the whole
send, as does steering or interrupting a session the agent did not spawn.
An ask still goes to exactly one recipient.

An older coordinator refuses a list ("cannot unmarshal array"); a send to
one address reads the same to it.

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

Kinds: `notify`, `ask`, `reply` (with `re=<ask id>`, and the first 400 bytes
of that ask quoted with `> ` under the header, also as `re_text` in
`fleet_inbox`), `done` (completion
notice), `system` (delivery failures and expiry, and a turn cut off by a
node or Codex restart: see [harnesses](harnesses.md#turns-cut-off-by-a-restart)). A line in a relayed
message that looks like a header is prefixed with `(quoted) `, so no
sender can put a header of its own in its text. The node matches a
line's skeleton, not its bytes: leading spaces and markup (`>`, `*`,
`-`, backticks), spaces and invisible characters (zero-width, format,
combining marks, blank fillers) are skipped, case is ignored, and
lookalikes are folded to the letters they pass for (fullwidth forms,
Cyrillic and Greek letters such as `ѕ` and `і`, mathematical, circled
and small capital letters, accented letters, `1` and `0`, and brackets
such as `［` and `【`). So `［switchworks`, `[ѕwitchworks` and
`[ switchworks` are quoted too. Nodes before 0.1.5 catch only `[switchworks`
in any case after leading spaces.

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

A message with files names the receiving node's copy of each
([Files](#files)):

```
[switchworks msg=01M42… from=winbox/port kind=notify]
(1 file attached: /home/dev/.cache/switchworks/files/01M3Z…/01M42…-7NLPG…/0001-fix-parser.patch (text/x-patch, 12 KiB).)
please git am this and push
```

A plain message from the owner has no header, but the line naming its
files comes first all the same.

## Inbox

Every message reaches its agent as a turn (or as part of one), so agents
need not poll `fleet_inbox`, and the tool's description tells them so.
It is there for an agent that wants to look back:

- By default it lists the messages to the session that it has not seen
  yet, oldest first, at most `limit` (default 20, max 200; `more` says how
  many are left). The coordinator keeps a cursor per session, the newest
  message the session has seen, which moves on when `fleet_inbox` returns
  messages and when a message is delivered as a turn. Only messages that
  went into the session's input are listed: not ones still pending (in
  flight, or a reply held for a waiting `fleet_ask`) or waiting in the
  session's queue (they arrive as a turn), not ones cancelled, expired or
  failed before they reached it, and not replies handed straight to a
  waiting `fleet_ask`. With nothing new it returns `{"messages": []}`.
- `all: true` lists recent messages to and from the session, newest
  first, seen or not, and does not move the cursor. Of the messages to the
  session it too lists only those that went into its input.
- Each message is compact: `{id, kind, from, created_at, text, re}`
  (`re` on replies; on messages the session sent, `to`, `state`,
  `delivered_at` and `seen_at`, so an agent checks a message's
  [receipt](#receipts) with `all: true` instead of sending it again).
  `verbose: true` returns full records instead.
- Text is cut at 2,000 bytes, with a note such as `… [1000 more bytes
  cut]`.
- With `ticket` it reports only on that ask (and lists no messages), so a
  `fleet_ask` waiting on its ticket does not mark messages seen.

An older coordinator refuses `all` and `verbose` ("unknown field"); update
the coordinator before the nodes.

## Images

Messages (`fleet_send`, `fleet_ask`, `swk send`/`msg`/`ask`, the web UI)
and first prompts (`fleet_spawn`, `swk spawn`, the web UI) can carry
images, for example a game agent's screenshots for a reviewer agent on
another host.

**Limits.** PNG, JPEG, WebP or GIF only, at most 4 images per message,
5 MiB each and 10 MiB in all. The type is sniffed from the bytes (magic
numbers); a declared type that disagrees is refused. Each check runs on
the sender's node (or CLI or browser), again on the coordinator, and again
on the receiving node. As base64, a message's images are at most about
13.4 MiB (16.1 MiB with [files](#files)), well under the 32 MiB limit of
a node connection's line. The API accepts
bodies up to 20 MiB on the three endpoints that take images and 4 MiB
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
other sessions' tokens, `cli.json`, `~/.ssh`, a Codex login. So the
sender's node reads the file itself, and only:

- inside the session's working directory, as the node recorded it at
  spawn (not anything the agent can change); a session confined to a
  directory root (spawned by an agent) is checked against that root first;
- reached without any symlink or Windows junction below the working
  directory: the node opens the path one directory at a time, each through
  the one before it, and checks that each is the directory it looked at,
  so a component swapped for a symlink mid-read is refused;
- a regular file: not a symlink (even to a file inside), device, pipe or
  directory, and with no other hard links (on Windows the count comes
  from the opened file), since another name for it could be anywhere;
- never under Switchworks's own directories (the node's directory, the
  config directory with `cli.json`), even when the working directory
  contains them (a session working in your home directory);
- never under a place that holds credentials, wherever the working
  directory is: `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.azure`,
  `~/.config/gcloud`, `~/.kube`, `~/.docker`, `~/.netrc`,
  `~/.git-credentials`, `~/.npmrc`, `~/.pypirc`, `~/.cargo/credentials`,
  the GitHub CLI's config, the keyring, `~/.password-store`, agents'
  logins (`~/.codex` or `CODEX_HOME`, `~/.claude`, `~/.claude.json`,
  `~/.gemini`, `~/.antigravity`, goose's and opencode's), browser and
  mail profiles, and their macOS (`~/Library/Keychains`, `~/Library/Cookies`,
  ...) and Windows (`AppData\Roaming\Microsoft\Credentials`, ...)
  counterparts (the full list is `credentialPaths` in
  `internal/attach/deny.go`); nor under the directory of received files
  ([Files](#files)), so an agent cannot forward another session's; nor
  under any path a permission profile in Codex's `config.toml` denies
  (`"<path>" = "deny"` in a `[permissions.<name>.filesystem]` table, any
  profile, read for every request; inline tables are not read, so write
  the profile as the [harnesses.md](harnesses.md#codex-network-without-exposing-credentials)
  example does); nor under `attach_deny` in `node.json` (a list of
  absolute or `~/` paths);
- not named like a credential, anywhere: `.env` and `.env.*` (but not
  `.env.example`), `*.env`, `.envrc`, private SSH keys (`id_rsa`,
  `id_ed25519`, ... but not `.pub`), `auth.json`, `credentials`, and
  `.ssh`, `.aws`, `.netrc`, `.npmrc` and the like found inside a project
  (`SecretName` in `internal/attach/within.go`);
- when the working directory is your home directory or above it, not a
  dot-file or inside a dot-directory at all, since most tools keep their
  logins there. `attach_allow` in `node.json` lists files or directories
  exempt from this rule and the one before (never from a deny);
- an allowed image type by its bytes, within the size limits.

Directories and files are compared with the denied ones by identity as
well as by name, so another spelling of the name (case on macOS and
Windows, a symlinked parent) does not get past it. These rules stand in
for a sandbox's deny list because the node reads outside the sandbox; a
credential stored somewhere else under the working directory can still
be attached, so deny it with `attach_deny` (and in your Codex profile).

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

## Files

Messages (`fleet_send`, `fleet_ask`, `swk send`/`msg`/`ask` with
`--file`, the API) can carry text files: a `git format-patch` from an
agent that cannot push (Codex on Windows keeps `.git` read-only and has
no network) for one that can, a log, notes. The receiver gets an exact
copy on disk and its path, so no model copies bytes by hand.

**Limits.** At most 4 files per message, 1 MiB each and 2 MiB in all.
A file must be UTF-8 text without NUL bytes; anything else (archives,
binaries, images: send those as `images`) is refused. The type is
`text/x-patch` (`.patch`), `text/x-diff` (`.diff`), `text/markdown`
(`.md`), `application/json` (`.json`, when it parses) or `text/plain`; it
is decided from the bytes and the name, never taken from the sender. Each
check runs on the sender's node (or CLI), again on the coordinator, and
again on the receiving node. A message may carry images and files
together: as base64 that is at most about 16.1 MiB, still well under the
32 MiB line limit, and the API's limit on the endpoints that take them is
20 MiB.

**Agents attach files by path**, in `files`, exactly as they attach
images: the sender's node reads them with the same checks (inside the
session's recorded working directory, opened one component at a time
with no symlink or junction on the way, a regular file without other
hard links, never under Switchworks's own directories, a credential
location, the received files or `attach_deny`, not a `.env` or key
file, and no dot-files when working in your home directory; see
[Images](#images)). UTF-8 text is all a key or token file needs to be,
so these rules are what keeps one from leaving. `swk mcp` and the node's local API refuse a
`file_attachments` argument from an agent (in any letter case), and paths
never reach the coordinator. Files go to agent sessions only: a send to
`user:<name>` with files is refused, since the inbox shows none.

**Storage.** The coordinator keeps files as it keeps images: metadata
(name, type, size, SHA-256) in the message record (`files`), the bytes in
its attachment store under their digest, with the same retention, limits
and eviction (images and files share the store). One agent may send at
most 8 MiB of files a minute, besides its 20 MiB of images, and so may
each person's credential (the admin token, each owner or operate token),
whatever names it sends as.

**On the receiving node.** When the node accepts the message it writes
each file to `<files dir>/<session>/<message>-<random>/<name>` (folders
0700, files 0600; the name is the sender's, cut to its base name, made
safe, made unique within the message and kept clear of names Windows
reserves), and the message's header names the paths. `<files dir>` is
`files_dir` in `node.json`, by default `switchworks/files` in the user
cache directory: `~/.cache/switchworks/files` on Linux,
`~/Library/Caches/switchworks/files` on macOS,
`%LocalAppData%\switchworks\files` on Windows. Not the node's directory:
the Codex permission profile that keeps credentials from agents denies
reads of the whole Switchworks config directory (`~/.config/swk`, with
the node key; see [harnesses.md](harnesses.md#codex-network-without-exposing-credentials)),
so a sandboxed agent could not read files there. Not the session's
working directory either, where they would clutter a repository. Codex's
`read-only` and `workspace-write` sandboxes let commands read outside the
workspace, so the agent reads (and `git am`s) them where they are; a
profile must not deny the files directory. A node that has no cache
directory, and a node from before 0.1.5, do not offer files, and the
coordinator refuses files for their sessions before storing anything (and
fails a waiting message whose node has stopped offering them, rather than
deliver it without them).

A message's folder is removed 7 days after the message left the node's
queue (an hourly sweep keeps it while it waits there). Events
(`message.queued`, `message.delivered`) carry no file contents.

**Quota.** The node keeps at most `files_quota_mib` (default 1024 MiB)
of received files, all sessions together. A message whose files would
pass it is refused by the node and fails, with an error saying so, rather
than being delivered without them; files fit again as the sweep removes
old folders. `-1` turns the quota off.

**Who can read them.** Every session on a host runs as the same user, so
any agent that can run commands can list and read every other session's
received files, in this directory or wherever `files_dir` puts it; file
modes cannot separate one user's sessions (and making the session folders
0711 would only open them to other users). The random part of a
message's folder name keeps a path from being guessed from a message id
by a tool that reads named paths without listing directories, and agents
can never attach files from this directory, so one cannot forward
another's; neither is a boundary between sessions. Treat a received file
as readable by every agent on the receiving host, and do not send
secrets as files.

An older coordinator refuses a message with files ("unknown field"), so
update the coordinator first, then the nodes: each node offers files once
it runs this version.

## Delivery

- Every message is stored before delivery is attempted.
- A message to a host that is offline stays **pending** and is delivered
  when the host reconnects (the router retries every 2 s). It expires after
  its TTL, and the sender gets a `system` notice. The TTL is 24 h
  (`default_ttl_seconds`), except for an agent's notify (`fleet_send`),
  which is mostly news that goes stale: 2 h (`agent_notify_ttl_seconds`).
  A sender may set `ttl_seconds`, shorter or longer, up to
  `default_ttl_seconds`. Asks, replies, completion notices and an agent's
  `fleet_spawn` prompt keep the full default.
- The TTL also covers the session's queue. The coordinator gives the node
  each message's `expires_at`, and when the node builds a turn from the
  queue, or steers in messages that waited for another steer, it drops any
  past it: an expired message is never delivered and never starts a turn.
  The node records `message.expired`, the message's state becomes
  `expired`, and an agent that sent it gets the same `system` notice (an
  expired ask fails, like a cancelled one). People's messages have the
  24 h default; `ttl_seconds` sets a shorter one. Send now
  (promote) sends a message whatever its expiry. Nodes before 0.1.5
  ignore `expires_at` and keep a queued message until it is delivered.
- The node deduplicates by message id, so redelivery after a reconnect has
  no effect. It keeps each id for as long as the coordinator could
  redeliver the message: until an hour past its `expires_at`, or 25 h
  for a message without one (from a coordinator before 0.1.5), and at
  most 4096 ids per session. Nodes before 0.1.5 kept the last 512.
- A session's queue holds at most 256 messages and 4 MiB of text, and a
  node's sessions at most 256 MiB of queued images. The owner's plain
  messages may use all of it; everyone else's (other people's, agents',
  Switchworks's notices) three quarters, so agents filling a queue never
  lock the owner out. A message that does not fit is refused: it
  becomes `failed` with an error saying the queue is full, and the
  sender can send it again later. A message the node cannot save to its
  `state.json` (a full disk) is refused the same way, rather than kept
  in memory only.
- Delivery modes:

  | Mode | Target running a turn | Target idle |
  | --- | --- | --- |
  | `auto` (default for `fleet_send`, `swk send`/`msg`, the web UI and the API) | into that turn (Codex `turn/steer`), seen at its next tool call | starts a turn |
  | `queue` | waits for the turn to end, then starts the next one | starts a turn |
  | `steer` | into that turn, even if the session does not accept steering (for people, and an agent's own children) | starts a turn |
  | `interrupt` | goes to the head of the queue and cancels the turn | starts a turn |

  Asks, replies, completion notices and Switchworks's notices go in mode
  `auto` too, so they reach a running turn rather than wait it out. The
  returned mode says what happened (`steer` or `queue`). Harnesses that
  cannot steer (agy, ACP agents) queue instead, and a session spawned with
  `accept_steer: false` (`swk spawn --no-steer`) gets `auto` messages, and
  other agents' steers, queued: for a long build that must not be
  distracted. Nodes before 0.1.5 take `auto` as `queue`.
- Messages that arrive while a steer is being delivered go in together, as
  one steer input, each with its own header.
- A person's message sent in mode `auto` (the web UI's Now) into a running turn waits in the session's
  queue until the turn's next step (a tool call or message finishing), when
  the agent would read it anyway, so it can be cancelled until then. If the
  turn ends first, it starts the next one. Mode `steer`, Send now, and
  agents' messages go in at once.
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
  credential: the same API token by id, so a token made later under a
  revoked one's name gets none of its messages) or an owner (the admin
  token, an owner token, or a browser one of those signed in): `POST /v1/messages/{id}/promote` with `mode`
  `steer` (into the running turn; with none running it goes next, ahead of
  the queue) or `interrupt`, and `POST /v1/messages/{id}/cancel`, after
  which the node never delivers it. The node records `message.promoted`
  and `message.cancelled`; a cancelled message's state is `cancelled`, an
  agent that sent it gets a `system` notice, and a cancelled ask fails.
  `GET /v1/sessions/{id}/queue` lists a session's queue in delivery order,
  then the messages the coordinator still holds for it (`held`: its host is
  offline), which can be cancelled the same way. With the host offline it
  lists only those and sets `offline`; with a node before 0.1.5, which
  cannot list its queue, it sets `node_too_old`. Promote and cancel of a
  message in the node's queue need a node from 0.1.5 (an older one answers
  409). The web UI shows each as a dimmed bubble under the transcript, and
  says in the composer when the session's host is offline.
- A queued message is delivered when its session goes idle. If the harness
  still says it is busy (Codex reports the thread idle a moment before the
  turn ends), the node tries again once the turn ends, or after 1, 5 and
  then every 30 s. Every 15 s it also looks for sessions that have been
  idle with queued messages for 30 s, logs a warning and tries again, and
  drops expired messages from every queue, also while its session runs
  a long turn or waits for an approval.
- If the harness refuses a turn of several messages (for any reason but
  being busy), each is tried again on its own, and only a message that
  fails alone becomes `failed`; one whose stored images are gone fails
  before the others go. If the harness does not answer in time (60 s
  for a turn, 30 s for a steer), whether the message went in is unknown:
  Codex may still start the turn. It is then neither sent again nor
  failed; the session's log gets a warning, and if the turn does start,
  its messages are reported delivered and then seen as usual.
- Delivery is at most once across a node crash: the node saves a turn's
  messages as being sent before the harness gets them (and a steered
  message's id before it goes in). One still marked so when the node
  starts again may or may not have arrived, so it is reported `failed`,
  with an error saying so, rather than delivered a second time.
- When a session stops, the messages in its queue become `failed` ("the
  session was stopped before it was delivered"), so their senders hear,
  and once the coordinator has all its events the node forgets the
  session: its record, spool, images and its received files (after the
  usual week). Nodes before 0.1.5 kept a stopped session's record and
  queue for good.

### Superseding

A sender that reports progress can give each update the same
`supersedes` key (`fleet_send`, `POST /v1/messages` and
`POST /v1/sessions/{id}/messages`, `swk msg`/`send --supersedes`), such as
`status:gate5`: at most 128 bytes, chosen by the sender. When a message
with a key is sent, each earlier message with the same key from the same
sender (for a person, the same credential, not only the same name) to the
same session that has not reached the session yet is replaced: one the
coordinator still holds (`pending`) is never sent, and one waiting in the
session's queue is dropped by the node (with `POST /v1/messages/{id}/cancel`'s
`OpCancel`, naming the newer message, so `message.cancelled` carries
`superseded_by`). The old message becomes `superseded`, with
`superseded_by` set to the newer one's id, and its sender gets no notice
(it sent the newer one). This happens before the newer message is
delivered, so the two never share a turn, and only the latest status
reaches the agent. A message that has gone into a turn (steered in, or
delivered) is left as it is, as is one being handed to the node at that
moment, and one queued on a node that is offline or older than 0.1.5
(which cannot cancel). A node from 0.1.5 drops it but leaves
`superseded_by` out of `message.cancelled`, so its transcript says
"cancelled". A different key, sender or session never supersedes.
Keys are for notifies (and broadcasts, per recipient): an ask with one is
refused. An older coordinator refuses `supersedes` ("unknown field").

### Quiet messages

An acknowledgement ("ok", "host free", "taking macmini") that starts a
turn costs a whole turn: the target's model reads its entire context
again only to note it. A message sent with `quiet` (`fleet_send`,
`POST /v1/messages` and `POST /v1/sessions/{id}/messages`, `swk msg`/`send
--quiet`) never starts a turn of its own:

- If the target is running a turn, it goes in as an `auto` message does
  (steered in, or queued for a session that does not take steering).
- If the target is idle, it waits in the session's queue, and goes in
  together with the next turn something else starts (another message, or
  the owner's prompt), each with its own header. A batch always holds the
  message that started it, even when quiet ones ahead of it fill the
  batch's bounds.
- It still expires after its TTL, like any queued message, and the sender
  gets the usual `system` notice; Send now (promote) delivers it at once,
  starting a turn if need be.
- An idle session with only quiet messages queued is settled, and the
  node's stall sweep leaves it alone.

Its header carries `quiet=1` and a line saying it needs no answer:

```
[switchworks msg=01M40… from=devbox/lead kind=notify quiet=1]
(Quiet: for your information; it needs no answer, so do not reply just to acknowledge it.)
host free
```

The MCP instructions tell agents to send acknowledgements, FYIs and "host
free" notes quiet, and never to answer a message only to acknowledge it.
Only notifies (and broadcasts) can be quiet; an ask, or a quiet message in
mode `interrupt`, is refused. A node before 0.1.7 ignores `quiet` and
delivers the message as usual.

### States

| State | Meaning |
| --- | --- |
| `pending` | stored, waiting for the target's node (its host is offline, or a reply is held for a waiting `fleet_ask`) |
| `delivered` | the target's node took it: into its input (`delivered_at` set), or into the session's queue (no `delivered_at` yet; the web UI says "queued"); a person's inbox |
| `seen` | the turn that carried it has completed (`seen_at`, `seen_turn`, `seen_item`); see [Receipts](#receipts) |
| `answered` | an ask that has been replied to (whether or not it was seen first) |
| `expired` | not delivered within its TTL, at the coordinator or in the session's queue |
| `failed` | could not be delivered, or an ask whose target stopped |
| `cancelled` | taken out of the queue before delivery |
| `superseded` | replaced before delivery by a newer message with the same `supersedes` key (`superseded_by`); see [Superseding](#superseding) |
| `dismissed` | an ask to a person that a person cleared without an answer (`dismissed_by`, `dismissed_at`); see [Asks to people](#asks-to-people) |

`delivered` (for a message that is not an ask), `seen` (likewise),
`answered`, `expired`, `failed`, `cancelled`, `superseded` and `dismissed`
are final. An ask moves on from `delivered` or `seen` to `answered`, or to
`failed` if its target stops first; a `fleet_reply` to an ask that is
`cancelled`, `expired`, `failed` or `dismissed` (its sender has been told it
will not be answered), or already answered, is refused (409 through the API).
A delivery the node reports again, or only after the message was seen,
changes neither its state nor its `delivered_at`.

### Receipts

A sender can tell whether a message arrived without asking the agent,
which would cost a model call over its whole context:

- **Delivered.** `delivered_at` is when the message went into the
  target's input: at once for a steer (and `fleet_send`'s result then
  carries it), and for one that waited in the queue when the node starts
  the turn that carries it (`message.delivered`, recorded once the
  harness has taken the turn: when it starts, or when the harness
  answers). An agent model sees it on its next call.
- **Seen.** When that turn (or the running turn it was steered into)
  completes, the node records `message.seen` with the turn's id and the
  item id of its last agent message, and the message becomes `seen`, with
  `seen_at`, `seen_turn` and `seen_item`. A turn that fails or is
  interrupted leaves its messages `delivered`. An ask that was answered
  before its turn ended stays `answered`, with `seen_at` set.
- `fleet_inbox` with `all: true` shows the state, `delivered_at` and
  `seen_at` of the messages an agent sent, and `GET /v1/messages/{id}`
  returns the whole record. The web UI shows a receipt under a person's
  message in the transcript ("Delivered 14:02", then "Seen 14:05 ·
  reply", which scrolls to the reply), and the state in the Inbox and
  Traffic lists. Agents get no notice when their message is seen.

`message.seen` comes from nodes from 0.1.5; with an older node messages
stay `delivered`. An older coordinator ignores the event. With either, a
queued message's `delivered_at` comes from `message.delivered`, which
nodes have always recorded.

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

### Asks to people

A person answers an ask in the web UI's Inbox or with `swk reply`, or
clears it without an answer:

- **Dismiss** (`swk messages dismiss <id>`, `POST
  /v1/messages/{id}/dismiss`) ends the ask as `dismissed`, recording who
  (`dismissed_by`: the name and credential) and when. An asker still
  waiting (`fleet_ask`, `swk ask`, a ticket wait) gets `failed`
  "dismissed without an answer by …" at once, and the ask is marked
  `inline`. An asker whose wait has ended gets a `system` notice
  (`notice_id`) that no answer will come; it is held back for 8 seconds,
  and until the ask's hold window closes (its `swk mcp` may still collect
  the dismissal with its ticket, and then needs no notice). A later reply
  gets 409.
- **Undo** (`POST /v1/messages/{id}/undismiss`) reopens a dismissed ask
  only while its asker has not been told: no waiting asker was woken, it
  has not checked its ticket, and the notice has not gone out (it is
  withdrawn, `cancelled`). Otherwise it gets 409. The web UI offers Undo
  for 6 seconds after a dismissal.
- **Mark read** (`swk messages read <id>`, `POST /v1/messages/{id}/read`)
  takes an ask or a notify to a person out of the Inbox's new items and
  badge count, without ending an ask: a read ask can still be answered or
  dismissed. The mark is kept on the message by the coordinator
  (`read_at`, `read_by`), so every browser and client sees it. Each
  message to people has one mark, its recipient's, whoever set it.
  A coordinator upgraded from 0.1.5 or earlier marks the messages to
  people it already holds read (`read_by` "switchworks (upgrade)"), all
  but the asks still open, so they do not all show as new at once.

## Locks

Agents share scarce things: a host's GPU, a LAN game-server port, a whole
test host. Rather than message each other to ask whether one is free and
to announce taking and freeing it, they take a lease with `fleet_lock`:

```json
{"name": "host:macmini-m1:gpu", "ttl_seconds": 3600, "wait_seconds": 600, "note": "benchmark run"}
```

- **Names** are free text up to 200 bytes, one namespace for the fleet.
  Agree on a form such as `host:<host>`, `host:<host>:gpu` or
  `port:<ip>:<port>`.
- **Held or busy.** The answer is `{status: "held", lock: {name,
  lease_id, holder, since, expires, ...}}`, or `{status: "busy", lock}`
  naming the holder, since when, its note and its expiry.
- **Leases end** at their TTL (default 30 minutes, at most 6 hours), when
  the holder calls `fleet_unlock`, when its session stops, goes away or is
  removed, or when an owner breaks the lock. Locking again a lock you hold
  renews it for a new TTL (`renewed: true`), keeping the lease.
- **Waiting.** With `wait_seconds` (at most 1800), `fleet_lock` blocks
  until the lock comes free or the wait ends (then `busy`), like
  `fleet_ask`. Waiters get it in the order they asked.
- **Limits.** A session holds at most 32 locks; at most 64 calls wait for
  one lock.
- The table lives in the coordinator's memory: a coordinator restart drops
  every lease. A holder that locks again gets it back unless someone else
  took it first.

People see the locks with `swk locks [prefix]` or `GET /v1/locks` (read
scope). The owner ends a lease with `swk locks break <name>` or `DELETE
/v1/locks/{name}`: it needs the admin token, an owner token or an
admin-scope token. The lock goes to the next waiter, and the old holder
gets a `system` notice that it no longer holds it.

An older node does not forward the lock tools ("unknown operation"):
upgrade the agent's host.

## Reminders

An agent that has to wait for something slow (CI, a long build on
another host, a person) should not poll or sleep in a turn: each check
resends its whole context to the model. It sets a reminder with
`fleet_remind {in_seconds, text, id}` and ends its turn; the `swk mcp`
instructions say so, and to ask people with `fleet_ask` `wait_seconds: 0`
(a ticket; the answer arrives as a message) rather than holding a turn
open for the answer.

- The coordinator stores a reminder as a pending `system` message from
  `swk` to the session that set it (`reminder` is its id, `not_before`
  its due time) and delivers it then, in mode `auto`: into a running turn
  if there is one, as a turn of its own otherwise. The text starts
  `Reminder "<id>", which you set 10m ago with fleet_remind:`. Like any
  pending message it survives a coordinator restart; one due while its
  host is offline waits for the host until 24 hours (`default_ttl_seconds`)
  past its due time, then expires.
- One pending reminder per id per session: a new one with the id of a
  pending one replaces it (that one becomes `superseded`), so an agent
  can re-arm "check CI on #123" each time it looks. Without an id, each
  reminder gets its own (its message id). A session may have 32 pending;
  `in_seconds` is 60 to 86400; `text` at most 4 KiB, `id` 128 bytes.
- `fleet_remind {cancel: id}` cancels one (`cancelled`). When the session
  stops or goes, its pending reminders expire, with the reason.
- `swk reminders <session>` and `GET /v1/sessions/{id}/reminders` (read)
  list a session's pending reminders, soonest first. An owner can cancel
  one, by its id or message id: `swk reminders cancel <session> <id>`,
  `POST /v1/sessions/{id}/reminders/{rid}/cancel`. They are not in the
  session's queue (`/queue`), and an older coordinator refuses the tool
  ("unknown operation").

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
| `max_hops` | 0 (no limit) | longest chain of message-triggered turns; a send that would exceed it is refused. Off by default, so agents can keep messaging each other until they are stopped |
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
| `max_broadcast` | 8 | recipients of one broadcast, or one send to several addresses, from an agent |
| `allow_unsandboxed_spawn` | false | let a sandboxed agent spawn a harness that does not enforce sandboxes (ACP agents such as goose, and agy) |
| `allow_unapproved_spawn` | false | let an agent that asks for approval spawn agy, which never asks |
| `default_ttl_seconds` | 86400 | how long an undelivered message waits; also the longest `ttl_seconds` a sender may ask for |
| `agent_notify_ttl_seconds` | 7200 | how long an agent's undelivered notify waits unless it sets `ttl_seconds` (never longer than `default_ttl_seconds`) |

The coordinator keeps at most 256 MiB of messages (their text, and the
names and digests of their images and files; the bytes themselves are in
the attachment store, under its own limits); past that the oldest
delivered (answered or not), failed or expired ones are dropped first, and
stay dropped across a restart.

People are not rate limited. With `max_hops` set, hops count from the
messages that started the sender's current work: a person's message is hop
0, the agent's message to another agent is hop 1, and so on. A turn that
starts with several messages (a batch from the queue) counts from the
deepest of them, so a shallow message cannot shorten a long chain. A steer
into a running turn does not change its count (it did not start the turn),
and a person's message, steered or not, starts it over at 0. After a
coordinator restart it is rebuilt from the stored messages. A spawn counts
as a hop too, so a loop of spawns and completion notices ends at
`max_hops`. Without `max_hops` (the default), agents' messages are limited
only by the per-minute rate limits and `max_daily_tokens`, like Claude
Code's sessions messaging each other.

A session an agent spawns always inherits the agent's approval policy and
sandbox setting, but only Codex and agy enforce a sandbox: ACP agents such
as goose run with the user's full access. So a sandboxed agent may not
spawn one (even where `spawn_harnesses` allows it) unless
`allow_unsandboxed_spawn` is set. agy's sandbox restricts only its
terminal, not its edit tools, so for this agy counts as unsandboxed too:
an agent of another harness may spawn agy only with `danger-full-access`
itself, or with `allow_unsandboxed_spawn` set.

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
  enrolls and removes hosts and manages tokens. Only the admin token or
  an owner token (or a browser one of those signed in) may create an
  owner token, mark or unmark one, or revoke one: an `admin` token that is
  not the owner's cannot make itself one. A person's spawn is not
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

The fleet tools read the files an agent attaches (`images`, `files`) as
your user, outside the agent's sandbox, so they can reach files the
sandbox denies. The node narrows that: attachments are confined to the
session's working directory and never come from Switchworks's own
directories, the usual credential locations, the paths your Codex
permission profiles deny, `attach_deny`, `.env` and key files, or (in a
session working in your home directory) any dot-file
([Images](#images), [Files](#files)). That is a deny list, not a
sandbox: a secret kept elsewhere under the working directory can still
be sent, so give agents working directories without secrets, add other
secret paths to `attach_deny`, and do not run sessions in your home
directory when you can avoid it. Images an
agent receives are model input like any message text, and a crafted
image can carry instructions the same way text can. A received file is
as untrusted as the message that brought it: a patch can change anything
it applies to, hooks included, so review it before applying it outside a
sandbox. The receiving node writes files only into its own folder for
the message, under names it chose, and never runs them.

## CLI

```sh
swk msg buildbox/helper "the build is green"         # by address
swk msg --broadcast @role=reviewer "please look at PR 42"
swk msg --to devbox/a --to buildbox/b "rebased on main"   # several recipients
swk msg --image shot.png --image diff.png reviewer "these two frames"
swk msg --supersedes status:deploy lead "deploy 3/5 hosts done"   # replaces my earlier status:deploy still waiting
swk msg --quiet lead "done with macmini"           # never starts a turn: waits for lead's next one
swk send --file 0001-fix.patch linuxbox/ci "git am this and push"
swk ask lead "are you done with the parser?"     # waits for fleet_reply
swk inbox                                        # messages and open questions for people
swk reply <ask-id> "yes, go ahead"
swk messages dismiss <ask-id>                    # clear a question without an answer
swk messages read <id>                           # mark a message to a person read
swk messages [session]                           # traffic log
swk messages promote <id>                        # send a queued message now (--interrupt for one that cannot steer)
swk messages cancel <id>                         # take it out of the queue
swk reminders devbox/lead                        # reminders it set itself with fleet_remind
swk reminders cancel devbox/lead ci:123          # cancel one (owners)
swk usage                                        # tokens by host, model, day
swk locks                                        # resource locks agents hold (fleet_lock)
swk locks break host:macmini-m1:gpu              # end a lease (owner or admin)
```

## HTTP API

| Method | Path | |
| --- | --- | --- |
| POST | `/v1/messages` | `{to, text, kind: notify\|ask\|broadcast, mode: auto (default)\|queue\|steer\|interrupt, ttl_seconds, supersedes, quiet, wait_seconds, from, attachments, files}`; `to` may be a list (not for asks); `supersedes` and `quiet` not for asks |
| GET | `/v1/messages?session=&inbox=1&limit=` | newest first |
| GET | `/v1/messages/{id}` | the record, with `state`, `delivered_at`, and `seen_at`, `seen_turn`, `seen_item` once seen; `supersedes`, and `superseded_by` once superseded |
| POST | `/v1/messages/{id}/reply` | `{text, from}`: a person answers an ask; 409 if it was answered or ended (dismissed, cancelled, expired, failed) |
| POST | `/v1/messages/{id}/dismiss` | `{from}` (optional): end an ask to a person without an answer; returns the record; 409 if it is no longer open |
| POST | `/v1/messages/{id}/undismiss` | reopen a dismissed ask whose asker has not been told; 409 otherwise |
| POST | `/v1/messages/{id}/read` | `{from}` (optional): mark a message to a person read (`read_at`, `read_by`) |
| GET | `/v1/usage` | daily totals, by host, by model |
| GET | `/v1/locks?prefix=` | `{locks: [{name, holder, holder_id, note, since, expires, waiting}]}`, by name |
| DELETE | `/v1/locks/{name}` | break a lock (the name path-escaped): the owner's credential or admin scope; `{name, released, next}`; 404 if it is not held |
| POST | `/v1/sessions/{id}/messages` | `{text, mode (default auto), from, supersedes, quiet, attachments, files}`; routed (202 with `state` and `mode`) |
| POST | `/v1/sessions` | a spawn; `attachments` go with `prompt`; `accept_steer: false` keeps `auto` messages out of its running turns |
| GET | `/v1/attachments/{sha256}` | a stored image: `{mime, size, sha256, data}` (base64) |
| GET | `/v1/sessions/{id}/queue` | the session's queued messages in delivery order: `{session, offline, node_too_old, messages: [{id, from, kind, owner, images, position, text, via, created_at, expires_at, held}]}`; `position` counts those ahead; `expires_at` from nodes that enforce it, or the coordinator's for `held` messages |
| POST | `/v1/messages/{id}/promote` | `{mode: steer\|interrupt}` (default steer): deliver a queued message now; the sender or an owner |
| POST | `/v1/messages/{id}/cancel` | take a queued (or still pending) message out, so it is never delivered; the sender or an owner |
| GET | `/v1/sessions/{id}/reminders` | the session's pending reminders, soonest first: `{session, reminders: [{id, message_id, text, due_at, created_at, expires_at}]}` |
| POST | `/v1/sessions/{id}/reminders/{rid}/cancel` | cancel a pending reminder by its id or message id; owners only (403 otherwise, 404 if none is pending) |

Reading needs a `read` token; sending, asking, replying, promoting,
cancelling, dismissing and marking read need `operate`.
API tokens are managed with `POST /v1/tokens` (`{name, scope, ttl_seconds}`;
the secret comes back once), `GET /v1/tokens` and `DELETE
/v1/tokens/{id-or-name}`, all admin only. A request beyond a token's scope
gets 403 naming the scope it needs. Reading a stored image
needs `read`.

`attachments` is a list of `{name, mime, data}` with `data` in base64;
`mime` may be left out (it is sniffed). `files` has the same form, for
text files. Messages returned by the API carry
`attachments` and `files` without `data`.
