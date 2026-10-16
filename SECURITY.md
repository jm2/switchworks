# Security policy

## Supported versions

Switchyard is pre-release. Security fixes land on `main` and are included in the next tagged
release. Report vulnerabilities in `main` and the latest release.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for
[jm2/switchyard](https://github.com/jm2/switchyard/security/advisories/new). Do not open a
public issue or pull request for a security problem. If the reporting form is unavailable, open
an issue that says only that you have a security report and how you can be reached privately,
with no detail of the problem.

Include the affected component, a reproduction, and the impact you believe it has. Never include
real admin or API tokens, enrollment tokens, node keys, `cli.json` contents or provider API keys;
redact host names and paths you would rather not share.

## Deployment assumptions

Switchyard is built for one owner (or a small team) and their own machines, on a private network
or tailnet:

- Ports 7480 (API and web UI) and 7481 (nodes) belong on a private network or a tailnet, never
  the open internet. They are authenticated and encrypted, but not hardened for internet
  exposure.
- `http_listen`, when set, must be a loopback address for a reverse proxy on the same machine.
- An agent that can run commands as the node's user can read that node's key and session
  tokens, and any `cli.json` there; harness sandboxes limit writes and network, not reads.
  See the trust model in [`docs/messaging.md`](docs/messaging.md#trust-model).
- The admin token is kept off every machine where agents run (in a password manager, or on
  one trusted machine). Machines with agents carry at most a `read` or `operate` API token
  (`swyd token create`), which cannot enroll or remove hosts or manage tokens, and can be
  revoked on its own. An `operate` token can still spawn sessions with any options on any
  host, which is as much as a person at that CLI can do.
- The coordinator's directory (CA key, `coordinator.json`, the admin `cli.json`) is readable
  by anything running as its user. Do not run agents as that user on the coordinator's
  machine.
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
- The node's local API used by `swyd mcp`, and files the node writes into workspaces (agy's
  `.agents/mcp_config.json`, `.git/info/exclude`).
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

Switchyard is a volunteer project, so these timelines are best effort.
