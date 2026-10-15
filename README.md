# Switchyard (`swyd`)

A lean, self-hosted control plane for coding agents spread across several
workstations: one view of every session on every host, and commands
(spawn, message, steer, interrupt, approve, stop) that work across machines.
It manages harnesses; it is not one. Codex does the agent work through
`codex app-server`; Goose and other Agent Client Protocol (ACP) agents run
over ACP; Antigravity's `agy` CLI is supported experimentally. See
[docs/harnesses.md](docs/harnesses.md).

*"Switchyard" is a working name (it collides with NVIDIA-NeMo/Switchyard). The
name lives in `internal/version` plus the module path, so a rename is cheap.*

Pure Go, standard library only, one static binary per platform. No Node, npm
or Electron anywhere.

## Status

Milestones M0–M3 of the [design and implementation plan](https://claude.ai/code/artifact/05bb9e2b-e9c4-4a2a-b46a-bbb91235d342):

- **M0**: Codex app-server client, `swyd dev run`. Verified against real Codex
  0.160.0 on linux/amd64: streaming, approvals, interrupt, resume.
- **M1**: coordinator + node + CLI: mTLS enrollment, append-only event log,
  managed sessions across hosts, queue/steer delivery, approvals, survives
  node and coordinator restarts. Verified with real processes on
  linux/amd64. The other targets cross-compile but are untested.
- **M2**: agent-to-agent messaging. Every managed Codex session gets fleet
  tools over MCP (`fleet_list`, `fleet_send`, `fleet_ask`, `fleet_reply`,
  `fleet_spawn`, ...). Messages are routed by the coordinator across hosts,
  wait for offline hosts, and are bounded by hop, rate, spawn-depth and
  token-budget guards. Verified with two real Codex app-servers on two
  hosts. See [docs/messaging.md](docs/messaging.md).
- **M3**: a phone-first web UI served by the coordinator: the live fleet,
  session transcripts with approvals and a composer, an inbox for agents'
  questions, message traffic and token usage. See
  [docs/web-ui.md](docs/web-ui.md).
- **M5**: ACP agents. Goose is built in (`goose acp`); other ACP agents are
  configured by name. Turns, streaming, approvals, interrupt, crash
  recovery, resume across node restarts and fleet tools. Verified against
  goose 1.54.0, including fleet messaging between a Goose and a Codex
  session. Goose reaches DeepSeek, GLM and MiMo through their native
  Anthropic-compatible endpoints, with no translation proxy.
- **Antigravity (`agy`)**: built from the documented headless schema
  (long-lived `stream-json` sessions, tool steps, usage, sandbox flags,
  opt-in fleet tools). Tested against a fake only; not yet run against a
  real `agy`.

Not built yet: observed sessions (M4). See [docs/status.md](docs/status.md).

![Web UI on a phone](docs/img/web-ui.png)

## Quick start

On the coordinator host (e.g. t240):

```sh
swyd coordinator init --host t240.lan,10.0.0.5   # first name = what nodes dial
swyd coordinator run                              # API :7480, nodes :7481
```

On each workstation:

```sh
swyd node enroll "$(ssh t240 swyd coordinator enroll-token antec)"
swyd node run
```

From anywhere with the CLI config (`swyd coordinator cli-config` on the
coordinator prints it; save it as `~/.config/swyd/cli.json`):

```sh
swyd hosts
swyd spawn macmini-m1 --cwd ~/src/CroMagRally --name cmr-arm "build and run the tests"
swyd attach macmini-m1/cmr-arm            # live stream; Ctrl-C detaches
swyd send cmr-arm "also check the Windows linker flags"   # queued for the next turn
swyd send --steer cmr-arm "skip the docs for now"         # into the running turn
swyd approve cmr-arm <approval-id> accept
swyd ls
swyd stop cmr-arm
```

Session paths are `host/name`. A bare name works when it is unique across the
fleet.

Agents can now message each other. A prompt like "ask boxy/helper whether
the ARM build passes" makes Codex call `fleet_ask`. You see the traffic and
answer agents' questions from the CLI:

```sh
swyd messages                 # who said what to whom
swyd inbox                    # questions agents asked you
swyd reply <ask-id> "ship it"
swyd ask helper "status?"     # ask an agent yourself, wait for the answer
```

The same things are in the browser at `https://t240.lan:7480/`. Run
`swyd ui-link` to get a one-time sign-in link for your phone.
[docs/web-ui.md](docs/web-ui.md) covers the certificate and
`tailscale serve`.

## Harness requirements

- **Codex**: `codex` on PATH, signed in (or providers configured) on that
  host. Codex 0.160.0 only accepts providers that speak the **OpenAI
  Responses API** (`wire_api = "chat"` is rejected). MiMo offers it;
  DeepSeek and GLM need a translating proxy, or use Goose instead.
- **Goose**: `goose` on PATH with a provider configured (`goose configure`,
  or env in `node.json`). For DeepSeek, GLM and MiMo use goose's
  `anthropic` provider with the vendor's Anthropic-compatible host; see
  [docs/harnesses.md](docs/harnesses.md).
- **Other ACP agents**: name them under `acp` in `node.json`, e.g.
  `"opencode": {"bin": "opencode", "args": ["acp"]}`.
- **Antigravity**: `agy` on PATH and signed in once interactively. Headless
  mode needs an unlocked OS keyring. Approvals cannot be relayed: either set
  allow rules in agy's `settings.json`, or spawn with `--sandbox
  danger-full-access`, which passes `--dangerously-skip-permissions`.

Per-host harness settings and spawn defaults live in the node's
`node.json` (`codex.bin`, `codex.home`, `agy.bin`, `agy.per_turn`,
`agy.fleet_tools`, `acp`, `default_approval_policy`, `default_sandbox`).

## Smoke test without spending tokens

```sh
swyd dev mock-provider --codex-home /tmp/mock-home &    # fake Responses endpoint
swyd dev run --codex-home /tmp/mock-home --approval never "hello"
echo accept | swyd dev run --codex-home /tmp/mock-home --approval untrusted "RUN: touch x"
```

The mock also serves the Anthropic Messages API, so Goose runs against it
too:

```sh
ANTHROPIC_HOST=http://127.0.0.1:<port> ANTHROPIC_API_KEY=mock GOOSE_PROVIDER=anthropic \
  GOOSE_MODEL=claude-sonnet-4-5 swyd dev run --harness goose --approval untrusted "RUN: ls"
```

To smoke-test a whole node this way, point its `codex.home` at the mock home.
`swyd dev run --record FILE` captures the raw protocol stream of any harness
(use it to pin down agy's event schema).

## Building and testing

```sh
make test        # unit + in-process fleet tests, race detector
make test-codex  # plus the real-Codex integration test (needs codex on PATH)
# The real-Goose tests run whenever goose is on PATH (or SWYD_GOOSE_BIN is set).
make cross       # dist/swyd-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64.exe}
```

## Layout

| Path | What |
| --- | --- |
| `cmd/swyd` | the single binary |
| `internal/codex` | app-server JSON-RPC client and protocol subset, with a contract test against the committed schema |
| `internal/adapter/{codexad,acpad,agyad}` | harness adapters behind `internal/adapter` |
| `internal/acp` | ACP client and protocol subset; `acptest` is a scripted fake agent |
| `internal/harness` | harness registry (built-in and configured ACP agents) |
| `internal/node` | sessions, spool, delivery queue, coordinator link, local API for `swyd mcp` |
| `internal/coord` | node listener, event store, projection, message router, HTTPS API + SSE |
| `internal/mcp` | `swyd mcp`: the fleet tools as a stdio MCP server |
| `internal/fleet` | fleet operation arguments and results |
| `internal/web` | the web UI (embedded HTML, CSS, JS) |
| `internal/store` | storage interface + file-backed implementation |
| `internal/pki` | private CA, enrollment tokens |
| `internal/wire` | node <-> coordinator protocol |
| `internal/cli`, `internal/apiclient`, `internal/render` | CLI |
| `schema/codex/0.160.0` | `codex app-server generate-json-schema --experimental` output |
| `docs/` | findings, decisions, protocol notes, status |
| `contrib/` | service units |

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
