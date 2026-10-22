# Changelog

All notable changes to Switchworks are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Changed

- **Web messages steer by default** — The session composer now delivers into the running turn
  ("Steer now") unless you pick another mode. "Next turn" queued your message behind every message
  already waiting, which on a busy session meant hours. A session that can't take input mid-turn
  still queues it, and says so.
- **Queued messages go in as one turn** — A session's backlog is delivered as one turn, each
  message with its own header, instead of one turn (and one full-context model call) per message.
- **People's messages first** — The owner's messages, then other people's, wait ahead of agents'
  in a session's queue, so a backlog of agent traffic no longer holds them back for hours.

### Fixed

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

### Security

- **Go 1.27.3** — Builds use Go 1.27.3, which fixes GO-2026-6809 in `crypto/x509`.
- **Longer Codex deny list** — The permission-profile docs now list the other credential stores a
  sandboxed command can read (other agents' logins, package and cloud tokens, GnuPG, keyrings,
  browser profiles, Codex's own transcripts), note that Linux needs Codex 0.163, and explain why
  Windows nodes stay without sandbox network for now.

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

[Unreleased]: https://github.com/jm2/switchworks/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/jm2/switchworks/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/jm2/switchworks/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/jm2/switchworks/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/jm2/switchworks/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jm2/switchworks/releases/tag/v0.1.0
