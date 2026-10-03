# Switchyard (`swyd`)

A lean, self-hosted control plane for coding agents spread across several
workstations: one view of every session on every host, and commands
(spawn, message, steer, interrupt, approve, stop) that work across machines.
It manages harnesses; it is not one. Codex does the agent work, driven through
`codex app-server`. Antigravity's `agy` CLI is supported experimentally.

*"Switchyard" is a working name (it collides with NVIDIA-NeMo/Switchyard). The
name lives in `internal/version` plus the module path, so a rename is cheap.*

Pure Go, standard library only, one static binary per platform. No Node, npm
or Electron anywhere.

## Status

Milestones M0 and M1 of the [design and implementation plan](https://claude.ai/code/artifact/05bb9e2b-e9c4-4a2a-b46a-bbb91235d342):

- **M0**: Codex app-server client, `swyd dev run`. Verified against real Codex
  0.160.0 on linux/amd64: streaming, approvals, interrupt, resume.
- **M1**: coordinator + node + CLI: mTLS enrollment, append-only event log,
  managed sessions across hosts, queue/steer delivery, approvals, survives
  node and coordinator restarts. Verified with real processes on
  linux/amd64. The other targets cross-compile but are untested.
- **Antigravity (`agy`)**: experimental adapter on documented headless
  behavior only. Not yet run against a real `agy`.

Not built yet: agent-to-agent messaging and the MCP server (M2), web UI (M3),
observed sessions (M4), ACP agents (M5). See [docs/status.md](docs/status.md).

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

## Harness requirements

- **Codex**: `codex` on PATH, signed in (or providers configured) on that
  host. Codex 0.160.0 only accepts providers that speak the **OpenAI
  Responses API** (`wire_api = "chat"` is rejected). Check that GLM,
  DeepSeek, MiMo or others expose a Responses-compatible endpoint, or put a
  translating proxy in front of them.
- **Antigravity**: `agy` on PATH and signed in once interactively. Headless
  mode needs an unlocked OS keyring. Approvals cannot be relayed: either set
  allow rules in agy's `settings.json`, or spawn with `--sandbox
  danger-full-access`, which passes `--dangerously-skip-permissions`.

Per-host harness settings and spawn defaults live in the node's
`node.json` (`codex.bin`, `codex.home`, `agy.bin`,
`default_approval_policy`, `default_sandbox`).

## Smoke test without spending tokens

```sh
swyd dev mock-provider --codex-home /tmp/mock-home &    # fake Responses endpoint
swyd dev run --codex-home /tmp/mock-home --approval never "hello"
echo accept | swyd dev run --codex-home /tmp/mock-home --approval untrusted "RUN: touch x"
```

To smoke-test a whole node this way, point its `codex.home` at the mock home.
`swyd dev run --record FILE` captures the raw protocol stream of any harness
(use it to pin down agy's event schema).

## Building and testing

```sh
make test        # unit + in-process fleet tests, race detector
make test-codex  # plus the real-Codex integration test (needs codex on PATH)
make cross       # dist/swyd-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64.exe}
```

## Layout

| Path | What |
| --- | --- |
| `cmd/swyd` | the single binary |
| `internal/codex` | app-server JSON-RPC client and protocol subset, with a contract test against the committed schema |
| `internal/adapter/{codexad,agyad}` | harness adapters behind `internal/adapter` |
| `internal/node` | sessions, spool, delivery queue, coordinator link |
| `internal/coord` | node listener, event store, projection, HTTPS API + SSE |
| `internal/store` | storage interface + file-backed implementation |
| `internal/pki` | private CA, enrollment tokens |
| `internal/wire` | node <-> coordinator protocol |
| `internal/cli`, `internal/apiclient`, `internal/render` | CLI |
| `schema/codex/0.160.0` | `codex app-server generate-json-schema --experimental` output |
| `docs/` | findings, decisions, protocol notes, status |
| `contrib/` | service units |
