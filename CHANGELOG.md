# Changelog

All notable changes to Switchworks are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/jm2/switchworks/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jm2/switchworks/releases/tag/v0.1.0
