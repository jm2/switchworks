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
- **No retention yet.** Event files grow without bound (M6).
- **Approval prompts in `attach` are print-only.** Answer them with
  `swyd approve`.
- **`swyd attach --until-idle`** relies on seeing the turn start in the
  replayed window. Use a larger `--tail` for very chatty turns.
- **No LICENSE file yet.** Waiting on your call: GPL-3.0 like xmat, or
  another.

## Recommended before M2

1. Run `swyd dev run` on all four hosts, starting with antec-win, and record
   one agy stream.
2. Decide: private app-server per node (current) or Codex's shared daemon
   (enables M4 and `codex resume` interop, but self-updates).
3. Confirm Responses-API support for the GLM, DeepSeek and MiMo endpoints you
   plan to run through Codex.
4. Evaluate Codex's `thread/queue/*` as the per-session queue, versus the
   node's own queue.
5. Then M2: router, `ask`/`reply`, completion notices, the `swyd mcp`
   server and runaway guards. The seams already exist: message headers,
   the `from` field, `Parent` on sessions, and labels.
