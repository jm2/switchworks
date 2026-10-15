# Status

## What works

| Area | State | Verified how |
| --- | --- | --- |
| Codex app-server client | done | contract test against the 0.160.0 schema; real-Codex integration test (turn, approval round trip, resume across processes) |
| Codex adapter | done | fake-app-server tests: turns, busy, steer, interrupt, approvals incl. offered decisions, app-server crash + auto-resume, resume across adapters |
| `swyd dev run` | done | real Codex + mock provider: streaming, interactive approval, `--record` |
| `swyd dev mock-provider` | done | used by the above |
| PKI and enrollment | done | unit tests: mTLS accept/reject, fingerprint pinning, single-use and expiring tokens |
| Coordinator store | done | unit tests: idempotent appends, reopen, torn-write recovery, path reuse |
| Coordinator + node + CLI | done | in-process e2e over real sockets (spawn, queue, approvals, steer, interrupt, node restart, coordinator restart, contiguous log, delta coalescing); real processes with real Codex (spawn, attach, send, ls, node SIGTERM, coordinator SIGKILL, 52-event gap-free log) |
| Messaging router | done | unit tests: address resolution, rate/hop guards, ask/reply races, held replies, expiry, completion notices |
| `swyd mcp` | done | protocol tests (initialize, tools/list, tool errors, transient retry, two-phase ask); driven by real Codex 0.160.0 |
| Fleet messaging end to end | done | in-process two-host e2e through the fake app-server's MCP client (list, cross-host ask inline and late, @label ask, person ↔ agent asks, guard errors, spawn + completion notice, status, stop, offline message delivered exactly once, hop limit); two real Codex app-servers on two hosts doing `fleet_ask` / `fleet_reply` |
| Web UI | done | server tests (sign-in by token and one-time link, cookie flags, CSRF header, throttling, static files, CA download, fleet stream); e2e (fleet stream snapshot and updates, approvals and previews on session views, rebuilt after a coordinator restart); driven in headless Chromium at phone and desktop sizes, light and dark, against two real Codex hosts: approve a command, cross-host ask from the composer, answer an agent's question, traffic, usage, spawn form, no console errors. Dark-default refresh (sidebar on desktop, drawer on phones, bundled fonts) re-checked the same way |
| ACP client | done | ACP v1 schema (agentclientprotocol repo); traffic recorded from goose 1.54.0 |
| ACP adapter (Goose and others) | done | fake-agent tests: streaming, tool items (command, edit with diff, MCP, generic), reasoning, plans, approvals with every decision, cancel during approval, stuck agent killed, crash and reopen, `session/load` with replay suppressed, `session/resume`, no-reopen agents, mode/model/provider config, fleet tools auto-approved; real goose 1.54.0 against the mock's Anthropic endpoint: approvals, commands, usage, resume across processes, approval modes |
| Goose in the fleet | done | e2e with real goose: fleet_list, goose → Codex fleet_ask, Codex → goose fleet_ask, resume after node restart; web UI driven in headless Chromium at phone size (tool cards, approval card, approve) |
| Antigravity adapter | experimental | fake-agy tests on the documented schema: stream-input and per-turn modes, tool steps, cumulative usage, failures, interrupt and restart, sandbox flags, soft-deny warnings, fleet-tools config files; never run against real `agy` (the installer host is not reachable from the build sandbox) |
| Security review fixes | done | regression tests: a node cannot write outside the store, into another host's logs or onto another host's paths; replaced and removed host certificates are refused; agent spawns are confined (host, harness, cwd with symlinks resolved, inherited policies, reserved labels, rate limit); spawn loops reach the hop limit; steer/interrupt/broadcast/status limits for agents; per-client sign-in throttle; server-side sign-out; terminal escapes stripped from CLI output; agy workspace symlink and gitdir tricks refused. Known limit: an agent with a shell on a host can read that node's key (see docs/messaging.md, Trust model) |
| Cross-compilation | done | linux/amd64, linux/arm64, darwin/arm64, windows/amd64 build and vet clean |
| `swyd service` (user services) | Linux done; macOS, Windows written | systemd: unit rendering and quoting unit-tested, packaged units pass `systemd-analyze verify`; not run against a live user manager in the sandbox (no systemd there). LaunchAgent and logon-task code compiled and vetted only; the task XML is unit-tested on Windows CI |
| Status file (`status.json`) | done | unit tests; a real coordinator and node run with `--log-file` reported running and connected, and the file went away on SIGTERM |
| Linux tray (`swyd-tray`, StatusNotifierItem) | done, little real use | D-Bus codec round trips and talks to a real `dbus-daemon`; a fake StatusNotifierWatcher receives the registration, reads the item's properties, pixmaps, tooltip and menu layout, and clicks an item. Not yet looked at in a real panel |
| Windows tray (`swyd-tray.exe`) | written | compiled and vetted only; never run |
| macOS menu-bar app | written | Swift source parses (tree-sitter); never compiled or run (no macOS in the sandbox); built by the release workflow |
| Packages | `.deb`, `.rpm`, Arch built locally | `.deb` built with dpkg-deb and lintian-clean, installed and run; `.rpm` built from a source tarball with rpmbuild (no scriptlets); both Arch PKGBUILDs built with makepkg; winget manifests validated against the 1.6.0 schemas. The Inno Setup installer and the `.dmg` are built only by the release workflow |

Tests: `make test` (race detector, about 30 s). `make test-codex` adds the
real-Codex test.

## Verified on

- linux/amd64 (build sandbox), Codex 0.160.0. Everything above.
- linux/arm64, darwin/arm64, windows/amd64: **compiled only.**

## Known gaps and risks

- **Agents with a shell are inside the node's trust boundary.** The guards
  limit what agents do through the fleet tools. Nothing stops an agent that
  runs commands as your user from reading the node's key and other
  sessions' tokens: Codex's `workspace-write` and `read-only` sandboxes
  limit writes and network, not reads. Keeping those secrets from agents
  would need an OS boundary, such as running the node as a separate user.

- **Windows untested.** Job-object teardown, `codex.cmd` shim resolution,
  the logon task `swyd service install` registers, its graceful stop
  through a named event, the tray and the installer are unverified. winbox should be
  the first real host.
- **Codex daemon self-updates.** See m0-findings. If the node is later
  pointed at the shared daemon, the protocol can change under it.
- **Providers must speak the Responses API** to work through Codex 0.160.0.
  Goose avoids this with Anthropic-compatible endpoints; those endpoints
  are verified only through the mock, not with real keys.
- **Goose's ACP is experimental upstream**, and `PromptResponse.usage` is an
  unstable ACP field. Goose reports the same usage figure on consecutive
  turns, so per-turn figures are taken when the report does not grow.
- **ACP agents other than goose are untested** (opencode, Crush's ACP PR,
  claude-agent-acp): expect mode-name and tool-naming differences.
- **ACP prompts are text only**; no images, ACP terminals or client file
  access.
- **agy is untested against the real binary.** The parser follows the
  documented schema; tool parameter names (`CommandLine`, `TargetFile`)
  and MCP tool naming are informed guesses. The soft-deny notice on stderr
  is matched loosely.
- **agy fleet tools rely on environment inheritance** from agy to the
  MCP servers it starts (unverified), and they write
  `.agents/mcp_config.json` into the workspace and an allow rule into
  agy's settings (opt-in).
- **No retention yet.** Event files grow without bound (M6). Terminal
  messages are compacted after 14 days.
- **Hop counting is per session, not per conversation.** A session working
  on a deep chain that then gets an unrelated message starts again from that
  message's hop count. Good enough to stop ping-pong; not a causal trace.
- **Rate-limit buckets live in memory** and reset when the coordinator
  restarts.
- **Web UI tested in Chromium only.** Safari on iOS is the main target and
  has not been tried. Watch the composer with the on-screen keyboard and
  the sticky header.
- **The transcript view loads the last 400 events.** "Show the whole
  history" loads everything, which is slow for very long sessions.
- **No push notifications.** The tab title and inbox badge count what
  needs you, but only while the page is open.
- **Approval prompts in `attach` are print-only.** Answer them with
  `swyd approve`.
- **`swyd attach --until-idle`** relies on seeing the turn start in the
  replayed window. Use a larger `--tail` for very chatty turns.

## Recommended before real-world use

1. Run `swyd dev run` on all four hosts, starting with winbox, and record
   one agy stream (`--harness agy --record agy.jsonl`). Run one goose
   session per host with a real provider.
2. Try a real model on two hosts with a prompt that needs the other host
   ("ask buildbox/helper what `uname -m` says there"), and watch `swyd messages`.
3. Set `guards.max_daily_tokens` before letting agents spawn freely.
4. Open the web UI on the phone you will actually use, and install the CA
   or set up `tailscale serve`.
5. For M4, decide whether the node should use Codex's shared app-server
   daemon (which lets it observe sessions you start in the Codex TUI) or keep
   its own private app-server.
