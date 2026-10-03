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
| Web UI | done | server tests (sign-in by token and one-time link, cookie flags, CSRF header, throttling, static files, CA download, fleet stream); e2e (fleet stream snapshot and updates, approvals and previews on session views, rebuilt after a coordinator restart); driven in headless Chromium at phone and desktop sizes, light and dark, against two real Codex hosts: approve a command, cross-host ask from the composer, answer an agent's question, traffic, usage, spawn form, no console errors |
| Antigravity adapter | experimental | fake-agy tests only (documented event types); never run against real `agy` |
| Cross-compilation | done | linux/amd64, linux/arm64, darwin/arm64, windows/amd64 build and vet clean |

Tests: `make test` (race detector, about 30 s). `make test-codex` adds the
real-Codex test.

## Verified on

- linux/amd64 (build sandbox), Codex 0.160.0. Everything above.
- linux/arm64, darwin/arm64, windows/amd64: **compiled only.**

## Known gaps and risks

- **Windows untested.** Job-object teardown, `codex.cmd` shim resolution and
  the Task Scheduler setup in `contrib/` are unverified. antec-win should be
  the first real host.
- **Codex daemon self-updates.** See m0-findings. If the node is later
  pointed at the shared daemon, the protocol can change under it.
- **Providers must speak the Responses API** to work through Codex 0.160.0.
- **agy event schema is a guess** beyond the documented top-level types.
  `step_update` text extraction may need adjusting after a real capture.
- **No retention yet.** Event files grow without bound (M6). Terminal
  messages are compacted after 14 days.
- **Fleet tools are Codex-only.** `agy` has no per-run MCP injection that
  Switchyard can use yet, so agy sessions can receive messages but not send
  them.
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

## Recommended before M4

1. Run `swyd dev run` on all four hosts, starting with antec-win, and record
   one agy stream.
2. Try a real model on two hosts with a prompt that needs the other host
   ("ask boxy/helper what `uname -m` says there"), and watch `swyd messages`.
3. Set `guards.max_daily_tokens` before letting agents spawn freely.
4. Open the web UI on the phone you will actually use, and install the CA
   or set up `tailscale serve`.
5. For M4, decide whether the node should use Codex's shared app-server
   daemon (which lets it observe sessions you start in the Codex TUI) or keep
   its own private app-server.
