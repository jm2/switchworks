# Decisions and departures from the design doc

**Standard library only.** The build environment could not reach the Go
module proxy, so the module has zero third-party dependencies. That also fits
the lean, static-binary goal. Two design choices changed because of it:

- **Storage: file store instead of SQLite.** `internal/store.Store` is an
  interface. `FileStore` keeps metadata in an atomically rewritten
  `state.json` and each session's events in an append-only JSONL file with
  an in-memory seq index. Event writes are fsynced before the node is
  acknowledged. A torn final line is truncated on open. Adding a SQLite
  implementation (modernc.org/sqlite, pure Go) later is a drop-in.
- **Node link: NDJSON over mTLS instead of gRPC.** Same semantics (one
  long-lived bidirectional stream per node, node-initiated), with no protobuf
  toolchain. `ProtocolVersion` is in the hello so it can evolve.

**Codex types are hand-written, not generated.** The schema bundle is 5.5 MB
of `oneOf` unions that generators handle poorly. Switchyard declares only the
subset it uses, and a contract test checks every method, notification,
field and enum value against the committed schema. Upgrading Codex means:
regenerate the schema into a new directory, bump `codex.SchemaVersion`, and
fix what the test reports.

**One app-server process per node**, shared by all of that node's Codex
threads. If it dies, the adapter restarts it and resumes every live thread.
See m0-findings for the option of using Codex's own shared daemon instead.

**Delivery semantics.** A message queued with `queue` waits for the session
to be idle, then becomes a new turn. `steer` goes into the running turn and
fails if there is none. Messages from people are passed through verbatim;
messages from anything else get a one-line `[switchyard msg=… from=…]`
header, ready for M2.

**Events are numbered by the node, deduplicated by the coordinator.** The
coordinator stores an event only if its seq is above the session's last
stored seq. The node resends everything above the acked seq after any
reconnect. That gives at-least-once delivery and exactly-once storage. Token
deltas are coalesced on the node before they are numbered.

**Antigravity runs one process per turn.** This uses only documented flags
(`--output-format stream-json`, `--conversation`, `--model`,
`--dangerously-skip-permissions`) with the prompt on stdin. The long-lived
`--input-format stream-json` mode is faster, but its stdin schema is
undocumented, so it waits until a real capture shows the shape.

**Enrollment.** `swyd coordinator enroll-token <host>` mints a one-time,
expiring token. The token carries the coordinator's API address and the CA's
SHA-256 fingerprint, so `swyd node enroll` can trust the coordinator before
it has the CA. The node's certificate CN is its host id; the coordinator
ignores whatever subject the CSR requested.

**Admin access.** The CLI uses one bearer token, stored hashed in
`coordinator.json` and in plain text in the 0600 `cli.json`. It is a single
owner on a LAN plus Tailscale. Per-user tokens are not needed yet.
