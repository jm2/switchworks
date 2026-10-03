# Web UI (M3)

The coordinator serves a web UI on its API port: `https://<coordinator>:7480/`.
It is built for a phone first and works on a desktop browser too. It is plain
HTML, CSS and JavaScript embedded in the `swyd` binary: no build step, no
framework, and nothing loaded from anywhere else.

![Fleet, approval, inbox and traffic views](img/web-ui.png)

## Views

- **Fleet**: every host and its sessions, live. Shows status, the last thing
  each agent said, tokens, sessions waiting for approval, and questions
  agents have asked you.
- **Session**: the live transcript (messages, routed messages with their
  sender, commands, tool calls, file changes, turn results). Pending
  approvals sit above the composer, with the decisions the harness offers.
  The composer sends with **Queue**, **Steer** or **Interrupt**, or as an
  **Ask** whose answer lands in your inbox. Interrupt and Stop are in the
  header.
- **Inbox**: questions agents asked people (`fleet_ask` to `user:…`), with a
  reply box each, and other messages addressed to people.
- **Traffic**: every routed message, filterable, with its delivery state.
- **Usage**: today against the daily budget, the last 14 days, and totals
  by host, model and session.
- **New session**: spawn on any online host.

Everything updates over two SSE streams: `GET /v1/stream` for the fleet
(a snapshot, then session, host and message updates) and the existing
per-session stream for transcripts.

## Getting to it

The API's certificate comes from the fleet's private CA, so a browser will
warn about it at first. There are two ways to fix that:

1. **Install the CA.** Open `https://<coordinator>:7480/ca.crt` and install
   it as a trusted certificate (iOS: Settings → General → About →
   Certificate Trust Settings after installing the profile). The server
   certificate covers the names you passed to `swyd coordinator init --host`.
2. **Use `tailscale serve`** for a real certificate on your tailnet name:

   ```json
   // coordinator.json
   "http_listen": "127.0.0.1:7482",
   "public_ui": "https://t240.your-tailnet.ts.net"
   ```

   ```sh
   tailscale serve --bg --https=443 http://127.0.0.1:7482
   ```

   `http_listen` serves the same API and UI over plain HTTP. Bind it to
   loopback only.

## Signing in

- **With the admin token**: paste the `token` from `cli.json` into the login
  page.
- **With a one-time link** (easier on a phone): run `swyd ui-link` anywhere
  the CLI works and open the link it prints. A link signs one browser in,
  once, within 15 minutes. The code is in the URL fragment, so it never
  reaches server logs or a `Referer`.

The browser gets a 30-day session cookie. Sign out from the top bar. To sign
every browser out, delete `ui.key` in the coordinator directory and restart
the coordinator.

## Security model

- The cookie is `HttpOnly` and `SameSite=Strict`, and `Secure` over HTTPS.
  It is HMAC-signed with `ui.key` (0600), so no server-side session table
  is needed.
- A cookie-authenticated request that changes anything must also send
  `X-Swyd-CSRF: 1`. A cross-site page cannot add that header without a CORS
  preflight, and the server never answers one.
- The pages are served with a strict CSP (`script-src 'self'`, no inline
  scripts or styles), `X-Frame-Options: DENY` and `Referrer-Policy:
  no-referrer`. Text from sessions is only ever inserted as text, never as
  HTML.
- Failed sign-ins are limited to 10 a minute.
- The CLI's bearer token works on every endpoint as before.

There is one identity, the owner. Messages sent from the UI come from
`user:web` and the CLI's from `user:cli`. Any person can answer a question
addressed to any `user:` name.
