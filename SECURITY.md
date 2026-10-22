# Security policy

## Supported versions

Switchworks is alpha software (v0.1). Security fixes land on `main` and are included in the
next tagged release. Report vulnerabilities in `main` and the latest release.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for
[jm2/switchworks](https://github.com/jm2/switchworks/security/advisories/new). Do not open a
public issue or pull request for a security problem. If the reporting form is unavailable, open
an issue that says only that you have a security report and how you can be reached privately,
with no detail of the problem.

Include the affected component, a reproduction, and the impact you believe it has. Never include
real admin or API tokens, enrollment tokens, node keys, `cli.json` contents or provider API keys;
redact host names and paths you would rather not share.

## Deployment assumptions

Switchworks is built for one owner (or a small team) and their own machines, on a private network
or tailnet:

- Ports 7480 (API and web UI) and 7481 (nodes) belong on a private network or a tailnet, never
  the open internet. They are authenticated and encrypted, but not hardened for internet
  exposure.
- `http_listen`, when set, must be a loopback address for a reverse proxy on the same machine.
- An agent that can run commands as the node's user can read that node's key and session
  tokens, and any `cli.json` there; harness sandboxes limit writes and network, not reads,
  unless a Codex permission profile denies them
  ([`docs/harnesses.md`](docs/harnesses.md#codex-network-without-exposing-credentials)).
  See the trust model in [`docs/messaging.md`](docs/messaging.md#trust-model).
- The admin token and owner tokens are kept off every machine where agents run (in a password
  manager, or on devices without agents). Machines with agents carry at most a `read` API
  token (`swk token create`), which only looks. An `operate` token there would let an agent do
  what a person at that CLI can: approve prompts, spawn sessions with any options on any host
  and send messages, which reach other agents with a header naming the token rather than as
  the owner's.
- The coordinator's directory (CA key, `coordinator.json`, the admin `cli.json`) is readable
  by anything running as its user. `swk coordinator init` copies the admin `cli.json` to that
  user's own CLI config only with `--user-cli` (or for the system service's account and the
  container image, which run no agents). Do not run agents as that user on the coordinator's
  machine. A rootless container under the same user is not a boundary (`podman unshare` and
  `podman exec` reach its volume); a separate account is, and on Linux and Windows `swk
  service install --system coordinator` sets one up. Anyone with root or administrator rights
  on that machine can still read it.
- The coordinator and the admin token control every node: whoever has either can start
  sessions and run commands, through the harnesses, on all enrolled hosts.

## What is in scope

- Authentication and authorization on the coordinator API, the web UI's sign-in, cookies and
  CSRF checks, and one-time sign-in links, including any way for a `read` or `operate` API
  token (or a browser or link it signed in) to do more than its scope allows, or to keep
  working after it is revoked or expires.
- The node protocol: mTLS, certificate pinning and revocation, enrollment tokens, and whether a
  node can read or change another host's sessions or the coordinator's files.
- The fleet tools: whether an agent can escape the guards in `coordinator.json` (spawn
  confinement, hop limits, rate limits, steer/stop/read rules) or pose as a person or another
  session.
- Message provenance: whether a message can reach an agent without a header (as the owner's)
  other than from the admin token, an owner token or a browser one of those signed in, or carry
  a credential (`via=`) other than the one it was sent with.
- The node's local API used by `swk mcp`, and files the node writes into workspaces (agy's
  `.agents/mcp_config.json`, `.git/info/exclude`).
- Image attachments: whether an agent can make its node read a file outside its working
  directory, or under Switchworks's own directories, through the fleet tools' `images`; whether a
  non-image (or an image of a type other than PNG, JPEG, WebP and GIF) gets through; and the
  coordinator's attachment store and `GET /v1/attachments`. See
  [`docs/messaging.md`](docs/messaging.md#images).
- Cross-site scripting or injection in the web UI, and terminal injection in CLI output.
- Secrets on disk (permissions, and anything written to logs or event logs).
- CI and release workflow permissions and their pinned actions.

## What is out of scope

- What an agent does with the access you gave it: commands it runs inside its own sandbox or
  approval policy, or on an unsandboxed session.
- Attacks that need the admin token, or a shell as the node's user (see the deployment
  assumptions); and what an API token's holder does within its scope.
- Vulnerabilities in Codex, Goose, agy or other harnesses; report those upstream.
- A coordinator exposed to the internet against the guidance above.
- Your network, firewall, tunnel or reverse-proxy configuration.

## What to expect

- An acknowledgement within a week.
- A fix, or an agreed plan, before any public disclosure; please allow time for it to land.
- Credit in the release notes if you want it.

Switchworks is a volunteer project, so these timelines are best effort.
