# Status

## What works

| Area | State | Verified how |
| --- | --- | --- |
| Codex app-server client | done | contract test against the 0.160.0 schema; real-Codex integration test (turn, approval round trip, resume across processes) |
| Codex adapter | done | fake-app-server tests: turns, busy, steer, interrupt, approvals incl. offered decisions, app-server crash + auto-resume, resume across adapters |
| `swk dev run` | done | real Codex + mock provider: streaming, interactive approval, `--record` |
| `swk dev mock-provider` | done | used by the above |
| PKI and enrollment | done | unit tests: mTLS accept/reject, fingerprint pinning, single-use and expiring tokens |
| Coordinator store | done | unit tests: idempotent appends, reopen, torn-write recovery, path reuse |
| Coordinator + node + CLI | done | in-process e2e over real sockets (spawn, queue, approvals, steer, interrupt, node restart, coordinator restart, contiguous log, delta coalescing); real processes with real Codex (spawn, attach, send, ls, node SIGTERM, coordinator SIGKILL, 52-event gap-free log) |
| Messaging router | done | unit tests: address resolution, rate/hop guards, ask/reply races, held replies, expiry, completion notices |
| `swk mcp` | done | protocol tests (initialize, tools/list, tool errors, transient retry, two-phase ask); driven by real Codex 0.160.0 |
| Fleet messaging end to end | done | in-process two-host e2e through the fake app-server's MCP client (list, cross-host ask inline and late, @label ask, person ↔ agent asks, guard errors, spawn + completion notice, status, stop, offline message delivered exactly once, hop limit); two real Codex app-servers on two hosts doing `fleet_ask` / `fleet_reply` |
| Web UI | done | server tests (sign-in by token and one-time link, cookie flags, CSRF header, throttling, static files, CA download, fleet stream); e2e (fleet stream snapshot and updates, approvals and previews on session views, rebuilt after a coordinator restart); driven in headless Chromium at phone and desktop sizes, light and dark, against two real Codex hosts: approve a command, cross-host ask from the composer, answer an agent's question, traffic, usage, spawn form, no console errors. Dark-default refresh (sidebar on desktop, drawer on phones, bundled fonts) re-checked the same way |
| ACP client | done | ACP v1 schema (agentclientprotocol repo); traffic recorded from goose 1.54.0 |
| ACP adapter (Goose and others) | done | fake-agent tests: streaming, tool items (command, edit with diff, MCP, generic), reasoning, plans, approvals with every decision, cancel during approval, stuck agent killed, crash and reopen, `session/load` with replay suppressed, `session/resume`, no-reopen agents, mode/model/provider config, fleet tools auto-approved; real goose 1.54.0 against the mock's Anthropic endpoint: approvals, commands, usage, resume across processes, approval modes |
| Goose in the fleet | done | e2e with real goose: fleet_list, goose → Codex fleet_ask, Codex → goose fleet_ask, resume after node restart; web UI driven in headless Chromium at phone size (tool cards, approval card, approve) |
| Images in messages and prompts | done | unit tests: type sniffing, limits, safe names, path confinement (outside the cwd, `..`, symlinked file, symlinked directory out, the node's key and directory inside the cwd, FIFOs), coordinator attachment store (retention, pinning, cap, digests), node queue files; Codex adapter writes private `localImage` files and removes them after the turn (fake app-server reads them); ACP adapter sends image blocks only when the agent advertises them; agy refuses; e2e: a screenshot file sent with `fleet_send` from one host arrives byte for byte in the other host's harness, plus `fleet_ask`, `fleet_spawn`, a person's `send` and spawn, refusals, metadata-only events and records; real goose 1.54.0 passes images to the mock's Anthropic emulation (by `swk send` and by a Codex agent's `fleet_send`). Web UI driven in headless Chromium (desktop and phone sizes) against a fake Codex host: attach with the file picker and by pasting, an SVG refused, send, thumbnails in the transcript and the inbox, the full-size view, no console errors or CSP violations |
| Changing a running session's approval policy | done | unit tests: the fake app-server receives the new policy on the next `turn/start`; the fake ACP agent switches mode; the node keeps it in its state and logs `session.policy`, a harness without it refuses; the coordinator rejects unknown values, keeps its record on a refusal and records the value it sent, not the node's. Real Codex 0.155 with the mock provider: under `untrusted` a command waited for approval, after `swk policy … never` the next ran unasked, and still did after a node restart. Web UI in headless Chromium: the Approvals menu changes it, the resizable sidebar (drag, arrow keys, double-click reset, kept after a reload, hidden on phones), no console errors |
| `swk dev vision-probe` | done | passes against the fake app-server, the fake ACP agent and real goose 1.54.0 through the mock provider; the mock reads the probe's PNG text chunk in both API emulations. Fleet test (2026-10-16): DeepSeek V4.1 Flash and MiMo V2.6 Pro see images in Codex and goose on Linux, macOS and Windows; both sometimes misread one character, which the probe now reports as a near miss |
| Antigravity adapter | experimental | fake-agy tests on the documented schema: stream-input and per-turn modes, tool steps, cumulative usage, failures, interrupt and restart, sandbox flags, approval-policy modes, soft-deny warnings, fleet-tools config files and their cleanup. Run against real agy 1.4.0 in the fleet test: streaming, follow-up turns, fleet_ask through MCP, resume after node restart, stop, image refusal; headless agy denies commands without allow rules (default, `auto` and `accept-edits` modes alike), and completed a coding task with permissions skipped |
| Security review fixes | done | regression tests: a node cannot write outside the store, into another host's logs or onto another host's paths; replaced and removed host certificates are refused; agent spawns are confined (host, harness, cwd with symlinks resolved, inherited policies, reserved labels, rate limit); spawn loops reach the hop limit; steer/interrupt/broadcast/status limits for agents; per-client sign-in throttle; server-side sign-out; terminal escapes stripped from CLI output; agy workspace symlink and gitdir tricks refused. Known limit: an agent with a shell on a host can read that node's key (see docs/messaging.md, Trust model) |
| Scoped API tokens | done | unit tests: every route declares a scope and an undeclared one needs admin (walks the route table), every route with read, operate and admin tokens, create/list/revoke by name or id, expiry, secrets never listed, persistence across a store restart, cookies carrying their scope (forged scope refused, pre-token cookies stay admin), cookies and pending links dying with their token, links capped at the minting token's scope, admin token unchanged; e2e: `swk token create --cli-config` makes a working operate `cli.json`, the CLI spawns, sends and mints links with it but `token ls`, `hosts rm` and enrollment are refused, a read token streams but cannot act, revoking cuts the CLI off |
| Cross-compilation | done | linux/amd64, linux/arm64, darwin/arm64, windows/amd64 build and vet clean |
| `swk service` (user services) | done | unit tests per platform (systemd unit quoting, LaunchAgent plist with `plutil -lint`, logon-task XML and SID names, the Windows stop event); CI's Services job runs a coordinator and node through systemd user units, LaunchAgents and logon tasks on GitHub's Linux, macOS and Windows runners: install, run as the user, connect, restart, stop, start, uninstall |
| Coordinator system service (`--system`) | done, little real use | unit tests: the system unit and its account match the packages', the environment `swk --system` passes, root-only binary checks, which commands `--system` allows, the Windows service command line; run in a Fedora 44 container booted with systemd, as a user with sudo, from a copied binary and from the RPM's unit: runs as `switchworks` under the unit's sandbox (exposure 1.4 in `systemd-analyze security`), its directory closed to the user, an operate token and a node as the user, restart, uninstall. CI's System service job does the same on GitHub's Linux and Windows runners (Windows: the virtual account and the folder's permissions), and installs and removes the `.deb` around it |
| Session retention (`swk rm`, `session_retention_days`) | done | unit tests: stopped and gone sessions past the retention are removed with their event logs and the web UI told; running, errored and recently ended ones stay, including one idle for long and only now marked gone (the end is the coordinator's clock); ancestors of a running session stay, and its spawn depth with them; a removed session's node cannot get it adopted again or reuse its id, and tombstones survive restarts and expire after a year; backlogs go in batches; a negative retention keeps all, a shorter one applies, an overflowing one is capped; removing a live session is refused; the route needs admin; event logs that cannot be removed are retried while running and at the next start. e2e: an operate token cannot `rm`, and an admin cannot remove a live session |
| Status file (`status.json`) | done | unit tests; a real coordinator and node run with `--log-file` reported running and connected, and the file went away on SIGTERM |
| Linux tray (`swk-tray`, StatusNotifierItem) | done, little real use | D-Bus codec round trips and talks to a real `dbus-daemon`; a fake StatusNotifierWatcher receives the registration, reads the item's properties, pixmaps, tooltip and menu layout, and clicks an item. Not yet looked at in a real panel |
| Windows tray (`swk-tray.exe`) | done, little real use | CI starts it on a Windows runner (it stays up; a second copy exits); not yet looked at on a real desktop |
| macOS menu-bar app | done, little real use | CI compiles it, packs and verifies the `.dmg`, and starts the app from /Applications; not yet looked at on a real desktop |
| Packages | all built in CI | `.deb` (lintian-clean, installed, units verified, the system coordinator run from it), `.rpm` (scriptlets create the account and handle only the system unit), Arch PKGBUILDs, the Inno Setup installer (installed and uninstalled per user in CI) and the `.dmg`; winget manifests validated against the 1.6.0 schemas |

Tests: `make test` (race detector, about 30 s). `make test-codex` adds the
real-Codex test.

## Verified on

- linux/amd64 (build sandbox), Codex 0.160.0. Everything above.
- Fleet test, 2026-10-16, built from source: a coordinator and five nodes
  (linux/amd64, linux/arm64 on Asahi, darwin/arm64, windows/amd64,
  windows/arm64) with Codex 0.155–0.160, goose 1.55 and agy 1.4.0 on
  DeepSeek V4.1 Flash and MiMo V2.6 Pro: enrollment, spawn, approvals,
  interrupt, resume after node restart, stop, usage, fleet_ask between
  hosts and to a person, the spawn guard, images between agents, scoped
  tokens and revocation. Packages, services and trays were not part of it.

## Known gaps and risks

- **Agents with a shell are inside the node's trust boundary.** The guards
  limit what agents do through the fleet tools. Nothing stops an agent that
  runs commands as your user from reading the node's key and other
  sessions' tokens: Codex's `workspace-write` and `read-only` sandboxes
  limit writes and network, not reads. Keeping those secrets from agents
  would need an OS boundary, such as running the node as a separate user.

- **Windows lightly tested.** Nodes built from source ran Codex and goose
  sessions on x86_64 and ARM64, and the v0.1.0 fleet test ran installed
  nodes from their logon tasks on both. The tray and the installer have
  had little use. Goose does not run on Windows on ARM.
- **Defender and the unsigned Windows binaries.** In that test Microsoft
  Defender quarantined `swk.exe` on both machines as
  `Behavior:Win32/Persistence.A!ml`, shortly after `swk service install`,
  which wrote a task file to `%TEMP%` and ran `schtasks.exe` on it. The
  task is now registered through the Task Scheduler API instead; that has
  not been tested against Defender. Code signing is the lasting fix. The
  README has the restore steps.
- **Interrupted commands.** Codex keeps an interrupted command running as a
  background terminal; Switchworks now stops the ones the interrupted turn
  started, and all of a session's on stop (Codex's experimental
  `thread/backgroundTerminals` methods; older app-servers leave them
  running until the node stops). goose on Windows can leave an interrupted
  shell command orphaned; it ends on its own when the command does.
- **Codex records trust for each working directory** in its own
  `config.toml` (`[projects."<cwd>"] trust_level = "trusted"`) the first
  time a session runs there, on Linux and macOS. Remove the entries for
  throwaway directories yourself.
- **Codex daemon self-updates.** See m0-findings. If the node is later
  pointed at the shared daemon, the protocol can change under it.
- **Providers must speak the Responses API** to work through Codex 0.160.0.
  DeepSeek and MiMo do (verified with real keys; MiMo needs
  `web_search = "disabled"`). Goose's Anthropic-compatible endpoints are
  verified only through the mock; its OpenAI-compatible providers were
  verified with real keys.
- **Goose's ACP is experimental upstream.** Token counts come from goose's
  unstable `_goose/unstable/session/update` notification (goose 1.55); if a
  goose release drops it, Switchworks falls back to `PromptResponse.usage`,
  which counts only the last model request of a turn.
- **ACP agents other than goose are untested** (opencode, Crush's ACP PR,
  claude-agent-acp): expect mode-name and tool-naming differences.
- **ACP prompts are text and images**; no audio, embedded resources, ACP
  terminals or client file access.
- **Images reach the harness, not necessarily the model.** Run
  `swk dev vision-probe` per host and model; goose drops images for a
  custom provider's model unless it is marked `"supports_vision": true`.
  agy cannot take images.
- **agy can do little headless without allow rules.** Its tools that need
  confirmation are denied, and only agy's real settings file
  (`~/.gemini/antigravity-cli/settings.json`) is read for allow rules. With
  permissions skipped it completed a coding task, slowly and with heavy
  token use, and read other conversations' transcripts under `~/.gemini`
  on the way.
- **agy fleet tools write outside the session** when enabled (opt-in):
  `.agents/mcp_config.json` in the workspace (removed when the session
  stops) and an `mcp(swk/*)` allow rule in agy's global settings (kept,
  with a warning when added). They are set up for new sessions only, not
  for sessions resumed after the setting changes. Environment inheritance
  from agy to `swk mcp` works with agy 1.4.0.
- **Running sessions grow without bound.** Ended sessions are deleted
  with their event logs 90 days after they ended by
  default (`session_retention_days`; negative keeps them), but a session that keeps running keeps its
  whole log. Terminal messages are compacted after 14 days.
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
- **The web UI does not know its scope.** A browser signed in with a
  `read` token still shows the composer and the approve and stop buttons;
  using them fails with a message naming the scope needed.
- **The admin token cannot be rotated by a command.** To replace it, put
  the SHA-256 (hex) of a new random secret in `admin_token_hash` in
  `coordinator.json`, restart the coordinator, and update the admin
  `cli.json`. Browsers it signed in stay signed in until `ui.key` is
  deleted.
- **No push notifications.** The tab title and inbox badge count what
  needs you, but only while the page is open.
- **Approval prompts in `attach` are print-only.** Answer them with
  `swk approve`.
- **`swk attach --until-idle`** relies on seeing the turn start in the
  replayed window. Use a larger `--tail` for very chatty turns.

## Recommended before real-world use

1. Run the release packages, services and trays on macOS and Windows; the
   fleet test covered binaries built from source only.
2. Set `guards.max_daily_tokens` before letting agents spawn freely.
   Move the admin token off every workstation that runs agents and give
   each one an `operate` or `read` token (`swk token create`).
3. Open the web UI on the phone you will actually use, and install the CA
   or set up `tailscale serve`.
4. For M4, decide whether the node should use Codex's shared app-server
   daemon (which lets it observe sessions you start in the Codex TUI) or keep
   its own private app-server.
