# Changelog

All notable changes to Switchworks are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Hop limit with fast agents** — A node sends a session's events ahead of that session's fleet
  requests, so the coordinator counts a message chain's hops from the message that started the
  turn even when the agent answers at once; before, a fast enough loop could outrun the limit.

## [0.1.5] — 2026-10-22

### Added

- **Codex config overrides** — `codex.config` in `node.json` is a map of Codex config overrides
  the node passes as `-c key=value` ahead of `app-server`, values written in TOML and nested
  objects as dotted keys, e.g. `{"model_auto_compact_token_limit": 128000}`.
- **Lean Codex sessions** — Opt-in `"codex": {"lean": true}` turns off Codex's apps (the GitHub
  connector among them), browser, computer use, image generation, plugins and web search, and
  compacts at 128K: a fresh thread's first call drops from 47.5K to 12.8K input tokens.
- **Degraded sandbox detection** — After a Codex thread starts or resumes, the node compares the
  sandbox Codex reports with the one asked for; a mismatch marks the session degraded (a warning
  and a status detail), and `caps.read_only` in `fleet_list` now follows the reported sandbox.
- **Store PowerShell warning** — On Windows the node warns at start when `pwsh` is the Microsoft
  Store alias, which Codex's sandbox can't start, and says to install the PowerShell 7 MSI.
- **Queue length** — Nodes report how many messages wait in each session's queue; `fleet_list`,
  `fleet_status`, the API and the web UI's session lists and details show it.
- **Queued messages in the web UI** — A message that waits for a session's next turn shows as a
  dimmed "Queued, N ahead" bubble with Send now and Cancel; agents' queued messages show as one
  row with Clear. `GET /v1/sessions/{id}/queue`, `POST /v1/messages/{id}/promote` and
  `POST /v1/messages/{id}/cancel` (the sender or an owner) do the same through the API.
- **Messages for offline hosts** — A message sent while the session's host is offline shows as
  a "Waiting for <host>" bubble with Cancel, instead of a toast, and the composer says the host
  is offline. The queue API lists these as `held`.
- **Messages to several recipients** — `fleet_send`'s `to` (and the API's, and `swk msg --to`
  repeated) takes a list of addresses: one message per recipient, sharing a group id, and a
  result that lists each one's id or why it could not go. Limits count every recipient.
- **File attachments** — Messages carry up to 4 text files (patches, logs; 1 MiB each): `files`
  in `fleet_send`/`fleet_ask`, checked like images, and `--file` on `swk send`, `msg` and `ask`.
  The receiving node writes them under `~/.cache/switchworks/files` and the message gives the paths.
- **Delivery receipts** — A message is `seen` once the turn that carried it completes, with the
  turn and its last reply; `fleet_send` returns `delivered_at` for a steer, and `fleet_inbox`
  (`all`) shows sent messages' state. The web UI shows "Delivered 14:02", then "Seen 14:05 · reply".
- **Low disk warning** — The coordinator checks the free space on its data volume every five
  minutes, logs a warning while it is low (under 5% of the volume, between 1 and 10 GiB), and
  reports `"storage_low": true` in `/v1/health`.

### Changed

- **Messages go into the running turn by default** — The new default mode, `auto`, for
  `fleet_send`, `swk send`/`msg`, the web UI ("Now (steer)") and the API, steers a message into
  the target's running turn when its harness can, and starts a turn otherwise. Agents may steer
  any session; interrupting still needs a session they spawned or `allow_steer_any`. Messages
  arriving during a steer go in as one. `swk spawn --no-steer` (`accept_steer: false`) opts out.
- **Queued messages go in as one turn** — A session's backlog is delivered as one turn, each
  message with its own header, instead of one turn (and one full-context model call) per message.
- **People's messages first** — The owner's messages, then other people's, wait ahead of agents'
  in a session's queue, so a backlog of agent traffic no longer holds them back for hours.
- **fleet_inbox lists unread messages** — By default it returns only messages the session has
  not seen (in an earlier call or as a turn), in a compact form, instead of the last 20 full
  records each time. `all: true` lists recent traffic and `verbose: true` gives full records.
- **Queued messages expire on the node** — A message's TTL (default 24 h) now also covers the
  session's queue: the node drops one past its `expires_at` instead of delivering it, the state
  becomes `expired` and an agent sender gets the usual notice. The web UI's queued bubble says when.
- **delivered_at means in the session's input** — A queued message's `delivered_at` is now set
  when its turn starts, not when the node queues it; until then it is `delivered` without one,
  which the web UI shows as "queued".
- **`coordinator init` keeps the admin config to itself** — It no longer installs the admin
  `cli.json` as your user's CLI config unless you pass `--user-cli` (the system service and the
  container image still do; they run no agents). `--no-user-cli` is still accepted. Setup hints
  now suggest a `read` token for every machine where agents run.
- **Admin-token browsers sign in again** — Browsers signed in with the admin token must sign in
  once more after upgrading (see Security), and so must a browser on HTTPS that still holds the
  old `swk_session` cookie.
- **Bounded session queues** — A session's queue holds at most 256 messages and 4 MiB of text,
  and a node's sessions 256 MiB of queued images; the owner's messages may use all of it, everyone
  else's three quarters, so agents can't lock the owner out. A message that doesn't fit, or that
  the node can't save, fails with the reason.
- **Stopped sessions are forgotten** — Stopping a session fails the messages in its queue, so
  senders hear, and once the coordinator has its events the node drops its record and spool.
- **At-most-once delivery across a crash** — The node saves a turn's messages as being sent before
  the harness gets them; one still marked after a restart is reported failed rather than
  delivered twice. Redeliveries are recognised until an hour past the message's expiry.
- **Event logs are capped** — Each session's event log on the coordinator is capped at 256 MiB
  (`event_log_max_mb`; negative for no cap). Past it the oldest events are dropped, keeping about
  half, behind a warning event; seq numbers don't change, so acks, stream resume and paging work
  as before. A long-running or looping session could fill the coordinator's disk.
- **Received files** — A node keeps at most `files_quota_mib` (default 1024 MiB) of received files,
  in `<session>/<message>-<random>/` folders. Sessions on one host run as the same user and can
  read each other's received files.
- **agy approval modes** — agy runs with `--dangerously-skip-permissions` only at sandbox
  `danger-full-access`; approval policy `never` below that maps to `--mode accept-edits`, and
  read-only sessions keep agy's request-review.

### Fixed

- **Delivery mode in the web UI** — The composer keeps Now (steer) as the default and keeps a
  session's other pick across sends and reopening the session, until the session ends.
- **Codex args without a subcommand** — `codex.args` is Codex's whole argument list; one with only
  `-c` overrides dropped `app-server` and every session failed with "connection closed". The node
  now refuses it at start, and logs app-server's stderr when it fails to start.
- **Read-only Codex sessions on Windows** — Codex ran `workspace-write` threads read-only on
  Windows when `config.toml` set no Windows sandbox mode, rejecting every command. The node now
  passes `-c windows.sandbox="unelevated"` unless `config.toml` or `node.json` sets one.
- **Stalled message queues** — A session could sit idle with hundreds of queued messages: when
  Codex reported the thread idle just before the turn ended, the node's delivery got "busy" and
  waited for an idle status that had already come. It now retries at once or with backoff, a
  sweep retries any session left idle with a queue (and logs a warning), and the Codex adapter
  drops a turn id that never completed once the thread has been idle for 10 s.
- **One bad message failed a whole batch** — If the harness refuses a coalesced turn, each message
  is retried on its own and only one that fails alone is failed. A turn or steer the harness
  doesn't answer in time is neither resent nor failed, and is reported delivered if it starts.
- **Expiry during long turns** — Queued messages now expire on time while their session runs a
  long turn or waits for an approval, and the coordinator no longer expires a message while its
  node is taking it.
- **Receipts** — `message.delivered` is recorded once a queued message has gone in, not before a
  busy attempt; a steered message is credited to the turn it went into.
- **Promote and image cleanup** — A queued message being sent now keeps its images while it is
  steered in.
- **Codex double turns** — A running turn is no longer dropped as stale because of an idle status
  from before it started, which could start a second turn beside it.
- **A harness that stops reading** — It no longer hangs every session on the node: a write that
  takes 30 s closes the connection and the node restarts the harness, and replies to the harness
  no longer block reading from it.
- **ACP and agy robustness** — An ACP agent that sends a request the moment it starts no longer
  risks crashing the node, and edit diffs are computed once; agy stdout lines are capped and its
  model is passed as `--model=value`; Codex notifications for unclaimed threads no longer fill
  the early-notification buffer for good.
- **Process groups** — On Linux and macOS, stopping a harness no longer signals a process group
  whose id may have been reused; on Linux its leftovers are ended before its process is reaped.
- **Store tidying** — Expired enrollment tokens are swept, the message budget also counts image
  and file metadata, messages dropped for it no longer come back after a restart, and an unknown
  `session.status` from a node is recorded as `error`. Sign-outs and other files written through
  `paths.WriteFile` survive a power loss (the directory is synced; not on Windows).

### Security

- **Go 1.27.3** — Builds use Go 1.27.3, which fixes GO-2026-6809 in `crypto/x509`.
- **Longer Codex deny list** — The permission-profile docs now list the other credential stores a
  sandboxed command can read (other agents' logins, package and cloud tokens, GnuPG, keyrings,
  browser profiles, Codex's own transcripts), note that Linux needs Codex 0.163, and explain why
  Windows nodes stay without sandbox network for now.
- **Other ways out of the Codex sandbox** — New docs: deny `/run/user/<uid>`, `/tmp/.X11-unix`,
  `/run/dbus` and `/run/libvirt` on Linux (through the session bus, `systemd-run --user` ran
  commands outside the sandbox), and turn connector apps off. Also covers what execpolicy allow
  rules expose, agents' own git tokens, Windows' default sandbox reading your secrets, and running
  GPU work safely.
- **Attachments could carry credentials** — Agents' attachments are read by the node as you,
  outside the sandbox, so an agent could send itself or another host keys its sandbox denied it.
  The node now refuses `images` and `files` from credential locations (`~/.ssh`, `~/.aws`,
  `~/.codex`, `~/.config/gh`, keyrings, browser profiles and more), CODEX_HOME, received files,
  the paths a permission profile in Codex's `config.toml` denies, and `attach_deny` in
  `node.json`; names like `.env` and private SSH keys anywhere; and dot-files when the session
  works in the home directory (`attach_allow` lifts those two). Windows refuses hard-linked
  files too. People's credentials may send at most 8 MiB of files a minute, like agents.
- **Hop limit across batches** — A turn that takes in several messages counts hops from the
  deepest of them, so a shallow message no longer resets a long agent-to-agent chain; a
  coordinator restart rebuilds the count from stored messages.
- **Inbox shows only what arrived** — `fleet_inbox` no longer lists messages to a session that are
  still queued, or were cancelled, expired or failed before reaching it; `fleet_reply` refuses
  an ask that was cancelled, expired or failed.
- **Owner powers** — Creating, marking or revoking an owner token takes the admin token or an
  owner token. Promote and cancel match a queued message's sender by API token id (`via_token`),
  so a new token under a revoked token's name cannot control its messages.
- **Valid tokens while throttled** — A valid token is accepted even while its client is throttled
  for failed attempts; only bad tokens get 429, so bad tokens from behind the same NAT, IPv6 /64
  or proxy no longer lock everyone there out. A sign-in link refused by its token's limit is no
  longer used up.
- **Admin cookies follow the admin token** — Replacing `admin_token_hash` signs out the browsers the
  admin token signed in. Over HTTPS only the `__Host-swk_session` cookie is accepted.
- **Header lookalikes** — Lines imitating Switchworks's message header with fullwidth, Cyrillic,
  Greek, mathematical or accented lookalikes, zero-width characters or a space after the bracket
  are quoted too. The node also checks spawn and message fields itself and refuses a second spawn
  for an id it already has.
- **Sessions looser than asked** — A Codex thread running with a looser sandbox than its session
  asked for is refused on spawn and resume instead of only being marked degraded, and one whose
  sandbox Codex doesn't report refuses an agent's spawn. An ACP spawn whose approval policy
  matches none of the agent's modes is refused instead of running in the agent's default mode.
  ACP permission options of unknown kinds are offered as `option:<id>`, and unprintable paths are
  quoted in diffs and approval prompts.
- **agy fleet tools and spawns** — agy no longer leaves a global `mcp(swk/*)` allow rule that
  pre-approved any MCP server named `swk`, including one an untrusted repository defines: each
  workspace's entry has a random `swk-…` name and its rule is removed when the workspace's last
  session ends. An agent may spawn agy only with `danger-full-access` itself or
  `guards.allow_unsandboxed_spawn`, since agy's `--sandbox` does not confine its edits.

## [0.1.4] — 2026-10-19

### Fixed

- **Harness tabs in Windows Terminal** — On Windows the node started codex, goose and agy with
  a console of their own, which Windows Terminal (the default terminal) showed as a tab in your
  window; closing it ended the harness. They now run with a hidden console.
- **Codex app-server restart loop** — An app-server that exits soon after it starts is restarted
  after growing waits and left down after 8 restarts in a row, instead of every second for
  good. The node log gives its exit code and how long it ran.

## [0.1.3] — 2026-10-19

### Added

- **Codex permission profiles** — `codex.permissions` in `node.json` names a Codex permission
  profile that `workspace-write` sessions run under, on start and on resume. A profile can turn
  the sandbox's network on while denying reads of credentials; Codex ignores profiles for a
  thread given a sandbox mode, so they had no effect on Switchworks sessions before.
  `swk dev run --codex-permissions` does the same for one turn.

### Security

- **Sandbox network and credentials** — The docs now warn against
  `[sandbox_workspace_write] network_access = true`, which lets sandboxed commands send out
  anything they can read (provider keys, SSH keys, the GitHub CLI's token, Switchworks tokens),
  and describe a permission profile that denies those reads instead.

## [0.1.2] — 2026-10-18

### Added

- **Change a running session's approval policy** — `swk policy <session> untrusted|on-request|never`,
  or the Approvals menu in the web UI's composer. Codex applies it from the next turn; Goose and
  ACP agents with session modes switch mode at once. It survives a resume. Loosening a policy
  needs the admin token or an owner token.
- **Resizable sidebar** — Drag the web UI sidebar's edge (or use the arrow keys on it) to see
  long session names; the width is kept per browser.

### Security

- **Messages from people name their credential** — A person's message reached agents without a
  header whichever token sent it, so an agent that read an `operate` token on its host could
  send messages that looked like its owner's. Messages and first prompts now carry the
  credential they were sent with (`via=token:<name>`), and only the admin token's and owner
  tokens' plain messages arrive without a header. Mark the tokens on your own devices with
  `swk token owner <name> on` (or `swk token create --owner`), and keep owner tokens off every
  host where agents run; those hosts should carry at most a `read` token.

## [0.1.1] — 2026-10-18

### Fixed

- **macOS node blocked from the LAN** — The LaunchAgent now starts the node through Switchworks,
  which macOS asks about Local Network access; v0.1.0's was denied without a prompt. Run
  `swk service install` again after updating.
- **Harnesses in `~/.local/bin` on Linux** — `swk service install` gives the node's user unit
  your login shell's `PATH`, so codex, goose and agy installed there are found.
- **Node dropped while connecting** — A command sent to a node in the moment between its
  connection and the coordinator's welcome no longer ends the connection.
- **Defender quarantined `swk.exe` on Windows** — `swk service install` wrote the logon task to a
  file in `%TEMP%` and ran `schtasks.exe` on it, which Microsoft Defender's behavior monitoring
  flagged as `Behavior:Win32/Persistence.A!ml`. Switchworks now uses the Task Scheduler API
  directly, with no task file and no child process; this is not yet tested against Defender.
- **`swk service start` and `restart` returned early on Windows** — They now wait for the node or
  coordinator to report running, so `swk service status` right afterwards shows it running.

### Known limitations

- **Defender may still flag the unsigned `swk.exe`** — If Windows Security quarantines it, restore
  it as the README describes and report the false positive to Microsoft.

## [0.1.0] — 2026-10-17

### Added

- **Coordinator and nodes** — One always-on coordinator keeps every session's event log, routes
  messages and serves the web UI and API; a node on each workstation runs the harnesses as you
  and dials out, so workstations open no inbound ports.
- **Codex sessions** — Driven through `codex app-server`, with streaming, diffs, token usage,
  approvals and steering of a running turn.
- **Goose and other ACP agents** — Goose through `goose acp`; other Agent Client Protocol agents
  configured by name (experimental).
- **Antigravity `agy` sessions** — Through its headless mode (experimental; agy 1.4.0).
- **One fleet view** — Every session on every host, addressed as `host/name`, from the web UI, a
  phone or the `swk` CLI.
- **Session control** — Spawn, message, steer, interrupt, approve, stop and resume on any host;
  sessions resume after a node or coordinator restart.
- **Fleet tools for agents** — `swk mcp` gives each session `fleet_ask`, `fleet_send`,
  `fleet_spawn` and friends, bounded by hop limits, rate limits, spawn depth and a daily token
  budget.
- **Inbox** — Agents can ask you questions; the tab title and badge count what needs you.
- **Images** — Screenshots in messages and prompts from the CLI, the web UI and between agents
  (Codex and Goose).
- **Web UI** — For desktop and phone, dark by default with Light and Match-system themes, and
  phone sign-in with a one-time link. It loads nothing from the internet.
- **Scoped API tokens** — `admin`, `operate` and `read` tokens (`swk token create`), with a
  ready-made `cli.json` for each workstation.
- **Session retention** — Ended sessions are deleted with their event logs 90 days after they
  end by default (`session_retention_days`); `swk rm` deletes one at once.
- **Services at login** — `swk service install` runs the node or coordinator as you: a systemd
  user unit, a LaunchAgent or a logon task.
- **System coordinator** — On Linux and Windows the coordinator can run as a service under its
  own account (`switchworks` or `NT SERVICE\swk-coordinator`), sandboxed, and starts at boot
  once it is set up.
- **Tray icon and menu-bar app** — Status, the web UI, start/stop/restart and logs on Linux,
  Windows and macOS.
- **Packages** — `.deb` and `.rpm` (x86_64 and ARM64), Arch (x86_64), a per-user Windows
  installer, a macOS `.dmg`, plain archives, and a coordinator image on GHCR.
- **Fake model provider** — `swk dev mock-provider` serves OpenAI Responses and Anthropic Messages, to try
  everything without spending tokens.

### Security

- **Private CA and mTLS** — Nodes and the coordinator authenticate each other with certificates
  from the coordinator's own CA; nodes join with one-time enrollment tokens.
- **Agent spawns confined** — By host, harness, working directory (symlinks resolved), inherited
  policies and a rate limit; a node cannot write another host's logs or paths.
- **No runtime dependencies** — Static Go binaries built from the standard library only.
- **Pinned workflow actions** — Every GitHub action in CI and the release workflow runs from an
  immutable commit; release files carry SHA-256 sums and build provenance.

### Known limitations

- **Agents with a shell share the node's trust** — They can read the node's key and other
  sessions' tokens on that host.
- **macOS and Windows** — The packages, services and trays have had little real use.
- **No macOS system coordinator** — Run it at login there, or on Linux or Windows.
- **macOS Gatekeeper** — The `.dmg` is ad-hoc signed but not notarized, so the first launch needs
  the one-time `xattr -cr` step from the README. The Windows installer is unsigned.
- **No push notifications** — The inbox count shows only while the page is open.
- **Running sessions grow without bound** — Only ended sessions are deleted.
- **Web UI tested in Chromium only** — Safari on iOS has not been tried.

[Unreleased]: https://github.com/jm2/switchworks/compare/v0.1.5...HEAD
[0.1.5]: https://github.com/jm2/switchworks/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/jm2/switchworks/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/jm2/switchworks/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/jm2/switchworks/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/jm2/switchworks/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jm2/switchworks/releases/tag/v0.1.0
