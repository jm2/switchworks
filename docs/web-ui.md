# Web UI

The coordinator serves a web UI on its API port: `https://<coordinator>:7480/`.
It is built for a phone first and works on a desktop browser too. It is plain
HTML, CSS and JavaScript embedded in the `swk` binary: no build step, no
framework, and nothing loaded from anywhere else.

![A session waiting for approval, on a desktop](img/web-ui.png)

## Layout

On a wide screen a sidebar holds New session, Inbox, Traffic and Usage,
every host with its sessions, a host filter and Settings. On a phone the
same sidebar opens from the menu button, the home screen is the host list,
and the composer is docked at the bottom of the screen.

The UI is dark by default. Settings switches between Dark, Light and Match
system; the choice is kept per browser.

## Views

- **Home**: every host and its sessions, live, newest first, with each
  session's last reply. Sessions waiting for approval and questions agents
  asked you are listed at the top. The composer at the bottom starts a
  session: describe the task, pick the host, harness and folder, and send.
  "More options" opens the full form.
- **Session**: the live transcript. Agent replies, your messages, messages
  routed from other sessions, and tool calls as cards (commands with their
  output, file changes with a diff, MCP tools, plans, thinking). Pending
  approvals sit above the composer with the decisions the harness offers.
  The composer delivers on the **next turn**, **steers** the running one,
  **interrupts and sends**, or **asks** with the answer going to your
  inbox. Interrupt and Stop are in the top bar; session details are at the
  top of the transcript. The image button (or pasting an image) attaches
  up to four PNG, JPEG, WebP or GIF images; images in messages show as
  thumbnails in the transcript, the inbox and the traffic list, and open
  full size when tapped. The home composer takes images for the first
  prompt the same way.
- **Inbox**: questions agents asked people (`fleet_ask` to `user:…`), with a
  reply box each, and other messages addressed to people.
- **Traffic**: every routed message, filterable, with its delivery state.
- **Usage**: today against the daily budget, the last 14 days, and totals
  by host, model and session.
- **New session**: every spawn option.

The UI embeds its fonts (Onest and JetBrains Mono, both SIL OFL 1.1; see
`internal/web/assets/fonts`) and loads nothing from the internet.

Everything updates over two SSE streams: `GET /v1/stream` for the fleet
(a snapshot, then session, host and message updates) and the existing
per-session stream for transcripts.

## Getting to it

The API's certificate comes from the fleet's private CA, so a browser will
warn about it at first. There are two ways to fix that:

1. **Install the CA.** Open `https://<coordinator>:7480/ca.crt` and install
   it as a trusted certificate (iOS: Settings → General → About →
   Certificate Trust Settings after installing the profile). The server
   certificate covers the names you passed to `swk coordinator init --host`.
2. **Use `tailscale serve`** for a real certificate on your tailnet name:

   ```json
   // coordinator.json
   "http_listen": "127.0.0.1:7482",
   "public_ui": "https://coord.your-tailnet.ts.net"
   ```

   ```sh
   tailscale serve --bg --https=443 http://127.0.0.1:7482
   ```

   `http_listen` serves the same API and UI over plain HTTP. It must be a
   loopback address; the coordinator refuses to start otherwise. Only on
   that listener, and only from loopback, does it trust the proxy's
   `X-Forwarded-Proto` and `X-Forwarded-For` headers.

## Signing in

- **With a token**: paste an API token (`swk token create phone --scope
  operate` prints one) or the admin token into the login page.
- **With a one-time link** (easier on a phone): run `swk ui-link` anywhere
  the CLI works and open the link it prints. A link signs one browser in,
  once, within 15 minutes. The code is in the URL fragment, so it never
  reaches server logs or a `Referer`.

The browser gets the scope of the credential that signed it in: the
token's, or for a link, the scope of the token that minted it (`swk
ui-link --scope read` asks for less; a link never gets more). A browser
with `read` scope sees everything live, but sending, approving, stopping
or spawning is refused with a message naming the scope it needs. See the
scope table in the [README](../README.md#3-the-cli-on-other-machines).

The browser gets a 30-day session cookie, or shorter if its token expires
sooner. Signing out from the top bar revokes that cookie on the server too,
so a copy of it stops working. `swk token rm <name>` signs out every
browser that token signed in, directly or through a link, and voids its
unused links. Pages and streams already open end within seconds of a
sign-out or `swk token rm`, and within 15 seconds of an expiry. Each
token may have 20 unused links at a time (a new one replaces the oldest)
and sign in 20 times a minute. To sign every browser out, delete `ui.key` in the
coordinator directory and restart the coordinator.

## Security model

- The cookie is `HttpOnly` and `SameSite=Strict`, and `Secure` over HTTPS.
  It is HMAC-signed with `ui.key` (0600), so no server-side session table
  is needed. The signed payload holds its expiry, a random id (for
  sign-out), its scope and the id of the API token behind it; each request
  checks that token still exists, so revoking the token ends the cookie.
  Open streams check again every 15 seconds, and at once after a sign-out
  or revocation.
- A cookie-authenticated request that changes anything must also send
  `X-Swk-CSRF: 1`. A cross-site page cannot add that header without a CORS
  preflight, and the server never answers one.
- The pages are served with a strict CSP (`script-src 'self'`, no inline
  scripts or styles), `X-Frame-Options: DENY` and `Referrer-Policy:
  no-referrer`. Text from sessions is only ever inserted as text, never as
  HTML.
- Images are shown as `data:` URLs (the CSP allows `img-src 'self' data:`
  and nothing else). The UI builds one only for PNG, JPEG, WebP or GIF,
  and only when the bytes' magic number matches that type: from files it
  read itself, or from `GET /v1/attachments/{sha256}`, which returns JSON
  (type sniffed again by the server, data in base64) rather than a raw
  image, so attachment bytes are never served as a page of the UI's
  origin. SVG, which can carry script, is never accepted.
- Failed token sign-ins are limited to 10 a minute per client. One-time
  links are not limited (they cannot be guessed), so a flood of bad tokens
  cannot keep you from signing in with a link.
- Sign-in and sign-out also require `X-Swk-CSRF: 1`. Over HTTPS the server
  sends `Strict-Transport-Security`; browsers ignore it until they trust
  the fleet CA, so it cannot lock anyone out.
- Bearer tokens and cookies go through the same check: every API route
  declares the scope it needs, and one without a declaration needs admin.

There is one identity, the owner; tokens only limit what each credential
may do. Messages sent from the UI come from
`user:web` and the CLI's from `user:cli`. Any person can answer a question
addressed to any `user:` name.
