# Switchyard (`swyd`)

A self-hosted control plane for coding agents running on several of your
machines. One place to see every agent session on every host, start new
ones, answer their approval prompts, and message them, from a browser,
your phone or the command line. Agents can also message each other across
machines.

Switchyard manages harnesses; it is not one. The agent work is done by the
tools you already use:

- **Codex**, through `codex app-server`
- **Goose** and other Agent Client Protocol (ACP) agents, through ACP
- **Antigravity's `agy` CLI** (experimental), through its headless mode

It is one static Go binary per platform with no runtime dependencies, and
its web UI loads nothing from the internet.

![A session waiting for approval](docs/img/web-ui.png)

## How it fits together

```
 browser / phone / swyd CLI
             │  HTTPS
     ┌───────┴────────┐
     │  coordinator   │  one always-on machine: event log, message router, web UI
     └───────┬────────┘
             │  mTLS, opened by each node
   ┌─────────┼──────────┐
 node      node       node       one per workstation, running as you
   │         │          │
 codex     goose       agy       the harnesses do the agent work, locally
```

- The **coordinator** stores every session's event log, routes messages
  and serves the web UI and API. It needs no harness installed.
- A **node** runs on each workstation as your user, so sessions use your
  sign-ins, repositories and toolchains. Nodes dial the coordinator; they
  open no inbound ports.
- Sessions are addressed as `host/name`, for example `devbox/api-server`.

## Install

Switchyard is built from source and needs Go 1.24 or newer.

```sh
git clone https://github.com/jm2/switchyard
cd switchyard
make build                     # ./swyd for this machine
install -Dm755 swyd ~/.local/bin/swyd
```

To build for your other machines from one place:

```sh
make cross   # dist/swyd-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64.exe}
```

Put the binary on each machine's `PATH`. The examples below use
`~/.local/bin/swyd` on Linux, `/usr/local/bin/swyd` on macOS and
`C:\tools\swyd.exe` on Windows.

## Set up

### 1. The coordinator

Pick a machine that is always on and that every workstation can reach. A
small home server is ideal.

```sh
swyd coordinator init --host coord.lan,10.0.0.5
```

`--host` lists the names and addresses the server certificate covers. The
first one is what nodes and the CLI will dial. Add a Tailscale name too if
you use one. `init` creates a private CA, the server certificate and an
admin token, and installs a CLI config for your user on this machine.

Start it, ideally as a service (the unit is in
[contrib/systemd](contrib/systemd/swyd-coordinator.service)):

```sh
swyd coordinator run
```

It listens on port **7480** (web UI, API and CLI) and **7481** (nodes).
Allow both through the firewall from your workstations.

### 2. Each workstation

Install and sign in to the harnesses you want to use there (see
[Harnesses](#harnesses)). Then enroll the machine with a one-time token
from the coordinator. The token carries the coordinator's address and CA
fingerprint, so there is nothing else to copy:

```sh
# on the coordinator
swyd coordinator enroll-token devbox
# on the workstation, with the token it printed
swyd node enroll <token>
```

or in one step over SSH:

```sh
swyd node enroll "$(ssh coord.lan swyd coordinator enroll-token devbox)"
```

The host name you give the token (`devbox`) is how the fleet names that
machine. Then run the node as your user, ideally as a service so it
survives logouts and reboots:

| OS | Service file |
| --- | --- |
| Linux | [systemd user unit](contrib/systemd/swyd-node.service) (enable lingering, as the file explains) |
| macOS | [LaunchAgent](contrib/launchd/dev.switchyard.node.plist) (set `PATH` so your harnesses resolve) |
| Windows | [logon task](contrib/windows/install-node-task.ps1) (runs in your session, so it can use your sign-ins) |

or in a terminal:

```sh
swyd node run
```

`swyd hosts` on the coordinator should now show the machine as online,
with the harnesses it found.

### 3. The CLI on other machines

The CLI works anywhere that can reach port 7480. Copy its config from the
coordinator:

```sh
ssh coord.lan swyd coordinator cli-config > ~/.config/swyd/cli.json
```

The file holds the admin token; keep it private (`chmod 600`).

### 4. The web UI

Open `https://coord.lan:7480/`. Because the certificate comes from the
fleet's own CA, the browser warns at first. Either:

- install the CA: open `https://coord.lan:7480/ca.crt` and trust it (on
  iOS: Settings, General, About, Certificate Trust Settings), or
- put the UI behind `tailscale serve` for a publicly trusted certificate
  (see [docs/web-ui.md](docs/web-ui.md)).

Sign in with the token from `cli.json`, or run `swyd ui-link` anywhere the
CLI works and open the one-time link it prints. That is the easy way to
sign in on a phone.

### 5. Your first session

In the web UI, describe a task in the box at the bottom of the home
screen, pick the host, harness and folder, and send. Or from the CLI:

```sh
swyd spawn devbox --cwd ~/src/myapp --name myapp "run the tests and fix what fails"
swyd attach devbox/myapp         # follow it live; Ctrl-C detaches
```

## Everyday use

```sh
swyd ls                                   # every session on every host
swyd send myapp "also update the changelog"          # next turn
swyd send --steer myapp "skip the docs for now"      # into the running turn (Codex)
swyd approve myapp <approval-id> accept   # or answer in the web UI
swyd interrupt myapp
swyd stop myapp                           # history is kept
```

A bare session name works when it is unique across the fleet.

Agents get fleet tools (`fleet_list`, `fleet_ask`, `fleet_send`,
`fleet_spawn`, ...), so a prompt like "ask buildbox/helper whether the ARM
build passes" makes one agent ask another on a different machine. You can
follow and join in:

```sh
swyd messages                 # who said what to whom
swyd inbox                    # questions agents asked you
swyd reply <ask-id> "ship it"
swyd ask helper "status?"     # ask an agent yourself and wait for the answer
```

Guards in `coordinator.json` bound agent-to-agent traffic: hop limits,
per-pair rate limits, spawn depth, which hosts agents may spawn on, and a
fleet-wide daily token budget (`guards.max_daily_tokens`). Set a budget
before letting agents spawn freely. See [docs/messaging.md](docs/messaging.md).

## Harnesses

Each node offers the harnesses it finds on that machine. Configure them in
the node's `node.json` (in `~/.config/swyd/node/` on Linux,
`~/Library/Application Support/swyd/node/` on macOS,
`%AppData%\swyd\node\` on Windows); restart the node after editing.

- **Codex**: `codex` on `PATH` and signed in, or with providers configured
  in its `config.toml`. Codex only accepts providers that speak the OpenAI
  Responses API.
- **Goose**: `goose` on `PATH` with a provider configured (`goose
  configure`, or environment variables in `node.json`). DeepSeek, GLM and
  MiMo work directly through their Anthropic-compatible endpoints.
- **Other ACP agents**: add them by name, for example
  `"acp": {"opencode": {"bin": "opencode", "args": ["acp"]}}`.
- **agy** (experimental): `agy` on `PATH`, signed in once interactively.
  Headless agy cannot ask for approval, so allow tools in its settings or
  spawn with sandbox `danger-full-access`.

[docs/harnesses.md](docs/harnesses.md) covers each one: approvals, models
and providers, fleet tools, and what each supports.

`node.json` also sets spawn defaults (`default_approval_policy`,
`default_sandbox`) and the harness binaries (`codex.bin`, `agy.bin`, ...).

## Try it without spending tokens

`swyd dev mock-provider` serves a fake model that speaks both the OpenAI
Responses API and the Anthropic Messages API, scripted by the prompt:

```sh
swyd dev mock-provider --codex-home /tmp/mock-home &
swyd dev run --codex-home /tmp/mock-home --approval untrusted "RUN: touch x"
```

`swyd dev run` drives one turn of any harness directly, without a node or
coordinator: a quick check that a harness works on a machine. With
`--record FILE` it captures the raw protocol stream.

## Limitations

- Developed and tested mostly on Linux. The macOS and Windows builds,
  service files and process handling have had little real use yet.
- The web UI has no push notifications; the tab title and inbox count show
  what needs you while it is open.
- Session event logs are kept indefinitely.
- The agy adapter follows agy's documented headless mode but has not been
  run against a real `agy`.

## Development

```sh
make test        # unit and in-process fleet tests, with the race detector
make test-codex  # also the real-Codex integration test (needs codex on PATH)
```

The real-Goose tests run whenever `goose` is on `PATH` (or `SWYD_GOOSE_BIN`
is set). Design notes are in [docs/decisions.md](docs/decisions.md); the
protocols are described in [docs/codex-app-server-notes.md](docs/codex-app-server-notes.md),
[docs/messaging.md](docs/messaging.md) and [docs/harnesses.md](docs/harnesses.md).

| Path | What |
| --- | --- |
| `cmd/swyd` | the single binary |
| `internal/codex` | app-server JSON-RPC client, with a contract test against the committed schema |
| `internal/acp` | ACP client; `acptest` is a scripted fake agent |
| `internal/adapter/{codexad,acpad,agyad}` | harness adapters |
| `internal/harness` | harness registry |
| `internal/node` | sessions, spool, delivery queue, coordinator link, local API for `swyd mcp` |
| `internal/coord` | node listener, event store, message router, HTTPS API and SSE |
| `internal/mcp` | `swyd mcp`: the fleet tools as a stdio MCP server |
| `internal/web` | the web UI (embedded HTML, CSS, JS, fonts) |
| `internal/store` | storage interface and file-backed implementation |
| `internal/pki` | private CA and enrollment tokens |
| `internal/wire` | node to coordinator protocol |
| `internal/cli`, `internal/apiclient`, `internal/render` | CLI |
| `schema/codex/` | `codex app-server generate-json-schema --experimental` output |
| `contrib/` | service files |

## License

Copyright (C) 2026 the Switchyard authors.

Switchyard is free software: you can redistribute it and/or modify it under
the terms of the GNU General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later
version. See [LICENSE](LICENSE).

The web UI embeds the Onest and JetBrains Mono fonts, licensed under the SIL
Open Font License 1.1; see `internal/web/assets/fonts`.

`schema/codex/` and `internal/codex/testdata/` contain output generated by
[openai/codex](https://github.com/openai/codex) (Apache-2.0); see
[schema/codex/README.md](schema/codex/README.md).
