<img src="internal/web/assets/icon.svg" width="96" alt="Switchworks icon">

# Switchworks

[![License: GPL-3.0-or-later](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![CI](https://github.com/jm2/switchworks/actions/workflows/ci.yml/badge.svg)](https://github.com/jm2/switchworks/actions/workflows/ci.yml)

A self-hosted **control plane for coding agents** spread across several of your machines,
written in dependency-free **Go**.

Switchworks shows every agent session on every host in one place, from a browser, your phone or
the `swk` command line. Start new sessions on any machine, answer their approval prompts, and
message them while they work. Agents get fleet tools too, so one agent can ask another on a
different machine for help. Switchworks manages harnesses; it is not one. The agent work is done
by the tools you already use, under your own sign-ins.

![A session waiting for approval](docs/img/web-ui.png)

> **Pre-release.** Switchworks is tested on Linux, and nodes built from source have run on
> macOS and Windows in a five-host fleet test; the macOS and Windows packages have had
> little real use. Per-area status and how each part was verified
> are in [`docs/status.md`](docs/status.md).

## Features

| Feature | Status |
| --------- | -------- |
| Codex sessions through `codex app-server` | ✅ |
| Goose sessions through the Agent Client Protocol (`goose acp`) | ✅ |
| Other ACP agents, configured by name | ⚠️ Experimental |
| Antigravity `agy` sessions through its headless mode | ⚠️ Experimental; run against agy 1.4.0, needs allow rules or full access to run commands |
| One fleet view of every session on every host | ✅ |
| Start, stop, interrupt and resume sessions on any host | ✅ Sessions resume after a node or coordinator restart |
| Approval prompts relayed to the web UI and CLI | ✅ Codex, Goose and other ACP agents |
| Steer a running turn | ✅ Codex; other harnesses queue the message for the next turn |
| Agent-to-agent messaging across machines (`fleet_ask`, `fleet_send`, `fleet_spawn`, ...) | ✅ Hop limits, rate limits, spawn depth and a daily token budget |
| Questions from agents to you, with an inbox | ✅ |
| Images in messages and prompts (screenshots between agents, from the CLI and web UI) | ✅ Codex and Goose (ACP agents that advertise image prompts); agy is text only |
| Web UI for desktop and phone | ✅ Dark by default, with Light and Match-system themes |
| Sign in on a phone with a one-time link | ✅ |
| Live streaming of turns, diffs and token usage | ✅ |
| Private CA, mTLS between nodes and coordinator, one-time enrollment tokens | ✅ |
| No runtime dependencies; web UI loads nothing from the internet | ✅ |
| Fake model provider for trying everything without spending tokens | ✅ OpenAI Responses and Anthropic Messages |
| Linux | ✅ |
| macOS, Windows | 🚧 Built and packaged; little real use yet |
| Packages: `.deb`, `.rpm`, Arch, Windows installer, macOS `.dmg` | 🚧 Built by the release workflow; little real use yet |
| Runs as your user at login: `swk service install` (systemd user unit, LaunchAgent, logon task) | ✅ Linux; 🚧 macOS, Windows |
| Tray icon (Linux, Windows) and menu-bar app (macOS) | 🚧 Status, web UI, start/stop/restart, logs; little real use yet |
| Push notifications | 🚧 Not yet; the tab title and inbox count show what needs you |
| Event log retention | 🚧 Session logs are kept indefinitely |
| Observing sessions started outside Switchworks | 🚧 Future release |

---

## Architecture

```text
   browser · phone · swk CLI
               │  HTTPS + server-sent events (port 7480)
     ┌─────────┴─────────┐
     │    coordinator    │  one always-on machine:
     │                   │  event log · message router · web UI
     └─────────┬─────────┘
               │  JSON lines over mTLS, opened by each node (port 7481)
   ┌───────────┼────────────┐
 node        node         node      one per workstation, running as you
   │           │            │
 codex       goose         agy      harnesses, over stdio
```

- The **coordinator** stores every session's event log, routes messages between sessions and
  people, and serves the web UI and API. It needs no harness installed.
- A **node** runs on each workstation as your user, so sessions use your sign-ins,
  repositories and toolchains. Nodes dial the coordinator and open no inbound ports; a node that
  loses its connection keeps its sessions running and replays their events when it reconnects.
- Each **harness** is driven over its own protocol: JSON-RPC on stdio for `codex app-server` and
  ACP agents, and agy's `stream-json` output. `swk mcp` gives each session its fleet tools.
- Sessions are addressed as `host/name`, for example `devbox/api-server`.

Design decisions are recorded in [`docs/decisions.md`](docs/decisions.md).

---

## Installation

Pre-built packages for Linux (`.deb`, `.rpm`, Arch), macOS (`.dmg`) and Windows (installer) are
available on the [Releases](https://github.com/jm2/switchworks/releases) page, with plain `swk`
archives for every platform. Download `SHA256SUMS.txt` with a package and check its SHA-256
entry before installing; `gh attestation verify <file> --repo jm2/switchworks` also checks a
file's build provenance.

**Nothing runs as root or SYSTEM, and the packages start nothing.** Each person turns on the
node for themselves with `swk service install`, which registers it with their own service
manager: a systemd user unit on Linux, a LaunchAgent on macOS, a logon task in Task Scheduler on
Windows. The node must run as you, so sessions get your sign-ins, repositories and toolchains.

The coordinator can run the same way, or, on Linux and Windows, as a **system service under an
account of its own** (`switchworks`, or the virtual account `NT SERVICE\swk-coordinator`), so
agents running as you on that machine cannot read its CA key, admin token or fleet state. The
Linux packages create that account and ship its unit disabled; see
[the coordinator](#1-the-coordinator).

### Supported platforms

| Platform | Architectures | Coordinator | Node | Packages |
| ---------- | --------------- | ------------- | ------ | ---------- |
| Linux | x86_64, ARM64 | ✅ | ✅ | `.deb`, `.rpm` (both architectures); Arch package (x86_64) |
| macOS | Apple silicon | 🚧 Little real use yet | 🚧 Built from source and run in a fleet test; needs Local Network permission | `.dmg` with the menu-bar app (macOS 13 or later) |
| Windows | x86_64 (ARM64 from source) | 🚧 | 🚧 Built from source and run in a fleet test on x86_64 and ARM64 | Per-user installer (`.exe`) |

### Fedora (COPR)

Switchworks will be available from the
[jmsqrd/switchworks](https://copr.fedorainfracloud.org/coprs/jmsqrd/switchworks/) COPR repository:

```bash
sudo dnf copr enable jmsqrd/switchworks
sudo dnf install switchworks
```

### Arch Linux (AUR)

[`switchworks-bin`](https://aur.archlinux.org/packages/switchworks-bin) will package the pre-built
release (x86_64 and aarch64):

```bash
yay -S switchworks-bin
```

### Windows (winget)

```powershell
winget install jm2.Switchworks
```

### Linux

| Distribution | Assets |
| --- | --- |
| Debian / Ubuntu | `switchworks-amd64.deb`, `switchworks-arm64.deb` |
| Fedora / RPM-based | `switchworks-x86_64.rpm`, `switchworks-aarch64.rpm` |
| Arch Linux | `switchworks-x86_64.pkg.tar.zst` |

```bash
sudo apt install ./switchworks-amd64.deb
sudo dnf install ./switchworks-x86_64.rpm
sudo pacman -U ./switchworks-x86_64.pkg.tar.zst
```

Each installs `swk` and `swk-tray` in `/usr/bin`, the `swk-node` and `swk-coordinator` user
units in `/usr/lib/systemd/user`, the coordinator's system unit
(`/usr/lib/systemd/system/swk-coordinator.service`, disabled) and a **Switchworks** launcher for
the tray icon, and creates the unprivileged `switchworks` account (no password, no login shell)
that the system unit runs as. The binaries are static. Removing the package stops and disables
the system coordinator but keeps its account and `/var/lib/switchworks`, which hold the CA.

### macOS

Apple silicon, macOS 13 or later: `switchworks-macos-aarch64.dmg`. Mount it and drag
**Switchworks** to Applications. The app is the menu-bar icon, and `swk` is inside it; put it on
your `PATH` for the setup steps:

```bash
mkdir -p ~/.local/bin
ln -s /Applications/Switchworks.app/Contents/MacOS/swk ~/.local/bin/swk
```

> **macOS note:** The `.dmg` is ad-hoc signed but not notarized, so Gatekeeper will block it on
> first launch. After dragging Switchworks to Applications, run:
>
> ```bash
> xattr -cr /Applications/Switchworks.app
> ```
>
> Then open normally. This is only needed once.

### Windows

`switchworks-windows-x86_64-setup.exe` installs for your user only, without administrator
rights, into `%LOCALAPPDATA%\Programs\Switchworks`: `swk.exe` (added to your `PATH`),
`swkw.exe` (the same program without a console window, which the node's logon task runs) and
`swk-tray.exe` (the notification-area icon, started at sign-in if you keep that option).
Uninstalling removes the logon tasks and keeps your configuration in `%AppData%\swk`. The
coordinator's system service, if you set one up, is separate: `swk service uninstall --system
coordinator` from an elevated prompt removes it.

### Podman (coordinator only)

The coordinator also ships as a container image, `ghcr.io/jm2/switchworks-coordinator`
(x86_64 and ARM64): `swk` alone on an empty base, running as an unprivileged user, with its
state in a volume. Nodes stay user services on the workstations, since sessions need your
sign-ins and toolchains. To run it rootless with Quadlet, as the account that should own it,
download `switchworks-coordinator.container` from the release:

```bash
mkdir -p ~/.config/containers/systemd
cp switchworks-coordinator.container ~/.config/containers/systemd/
systemctl --user daemon-reload
podman run --rm -v swk-coordinator:/data ghcr.io/jm2/switchworks-coordinator:latest \
  coordinator init --host coord.lan,10.0.0.5
systemctl --user start switchworks-coordinator
loginctl enable-linger "$USER"
```

The CLI runs inside it for the coordinator's own commands, for example
`podman exec switchworks-coordinator swk coordinator enroll-token devbox`, and
`podman exec switchworks-coordinator swk coordinator cli-config` prints the admin `cli.json`
(keep it in your password manager). The unit publishes 7480 and 7481, uses `pasta` so the
coordinator sees clients' real addresses, drops all capabilities, keeps the root filesystem
read-only, and restarts the container when its healthcheck (`swk coordinator health`) fails.
Back up the `swk-coordinator` volume: it holds the CA key.

A container under the same account as your agents is not a security boundary: they can reach
its volume with `podman unshare`. Give it an account of its own on a machine where agents run, or
use the [system service](#as-a-system-service), which does that for you.

### Archives

`swk-<version>-<os>-<arch>.tar.gz` (`.zip` on Windows) holds just the binaries. Put `swk`
on each machine's `PATH`, for example `~/.local/bin/swk` on Linux and macOS or
`C:\tools\swk.exe` on Windows.

## Building from Source

### Prerequisites

- Go 1.27 or newer (the `go` command fetches the exact toolchain in `go.mod`, 1.27.2, if yours is older;
  Fedora's `golang` package sets `GOTOOLCHAIN=local`, so build there with `GOTOOLCHAIN=auto make build`)
- `make` (optional; the targets are short `go build` commands)

```bash
git clone https://github.com/jm2/switchworks
cd switchworks
make build tray                  # ./swk, and ./swk-tray (Linux and Windows)
mkdir -p ~/.local/bin
install -m 755 swk ~/.local/bin/swk
install -m 755 swk-tray ~/.local/bin/swk-tray   # Linux only; macOS uses the Switchworks app
```

To build for your other machines from one place:

```bash
make cross   # dist/swk-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64.exe}, swk-tray, swkw.exe
make deb ARCH=amd64   # dist/switchworks-amd64.deb from those (needs dpkg-deb)
```

The packaging files are in [`build-aux/`](build-aux): the RPM spec (also used by COPR), the Arch
`PKGBUILD`s, the `.deb` script, the Inno Setup script, the winget manifest templates and the
macOS app build (`build-aux/macos/build-dmg.sh`, which needs Xcode's command-line tools).

---

## Setup

### 1. The coordinator

Pick a machine that is always on and that every workstation can reach. A
small home server is ideal.

```bash
swk coordinator init --host coord.lan,10.0.0.5
```

`--host` lists the names and addresses the server certificate covers. The
first one is what nodes and the CLI will dial. Add a Tailscale name too if
you use one. `init` creates a private CA, the server certificate and an
admin token, and installs a CLI config with that token for your user on
this machine (`--no-user-cli` skips that, for a machine where agents run).

The admin token controls the whole fleet. Keep a copy somewhere safe, such
as a password manager, and keep it off every machine where agents run (see
[step 3](#3-the-cli-on-other-machines)).

Run it as a service under your own user, so it starts at login:

```bash
swk service install coordinator
```

or in a terminal: `swk coordinator run`. On Linux, also run `loginctl enable-linger "$USER"`
so it keeps running while you are logged out (`swk service install` reminds you).

#### As a system service

If agents will run on the coordinator's machine too, run the coordinator under an account of
its own instead: anything running as you can read your files, the CA key and admin token
included. On Linux and Windows, `swk service install --system coordinator` sets that up:

| OS | The service | Its account | Its files |
| --- | --- | --- | --- |
| Linux | `swk-coordinator.service`, a system unit with systemd's sandboxing (no capabilities, read-only system, no home directories, a system-call filter) | `switchworks` (created by the package, or by the install) | `/var/lib/switchworks`, mode 700 |
| Windows | The `swk-coordinator` service, allowed through Windows Firewall on private and domain networks | `NT SERVICE\swk-coordinator`, a virtual account with no password | `%ProgramData%\Switchworks`, open only to that account, SYSTEM and administrators |

Setting it up takes root (or an elevated prompt on Windows, without `sudo`); the coordinator
itself never runs with either. It starts at boot only once it is set up: on Linux the unit is
skipped until `init` has written its config, and on Windows `swk service start --system` is
what turns starting at boot on. Do it instead of the steps above, not as well:

```bash
sudo swk service install --system coordinator
sudo swk --system coordinator init --host coord.lan,10.0.0.5
sudo swk service start --system coordinator
```

On Windows, from an elevated PowerShell:

```powershell
swk service install --system coordinator
swk --system coordinator init --host coord.lan,10.0.0.5
swk service start --system coordinator
```

`swk --system <command>` runs a coordinator or CLI command as the coordinator's account, with
its files and its admin `cli.json`: `sudo swk --system coordinator enroll-token devbox`, `sudo swk
--system token ls`, and so on. For the CLI as yourself, give your own account a scoped token:

```bash
mkdir -p ~/.config/swk
(umask 077; sudo swk --system token create "$USER" --scope operate --cli-config > ~/.config/swk/cli.json)
```

On Windows, make the token from the elevated prompt and save it where your own (non-elevated)
`swk` reads it; `%AppData%\swk` is private to your account:

```powershell
New-Item -ItemType Directory -Force "$env:APPDATA\swk" | Out-Null
$cfg = swk --system token create $env:USERNAME --scope operate --cli-config | Out-String
[IO.File]::WriteAllText("$env:APPDATA\swk\cli.json", $cfg)  # UTF-8 without a BOM
```

Where `swk` is not root's own (installed in your home directory, or the per-user Windows
install), the service runs a copy that only root or administrators can replace:
`/usr/local/lib/switchworks/swk` or `%ProgramFiles%\Switchworks Coordinator\swk.exe`. Run the
install again after upgrading Switchworks to update it; the Linux packages' own `/usr/bin/swk`
needs nothing, and a package upgrade restarts the service. `swk service
start|stop|restart|uninstall --system coordinator` manage it, and `swk service status` shows
it. Uninstalling keeps the account and the coordinator's files. macOS has no system service
yet; give the coordinator a user account of its own there.

It listens on port **7480** (web UI, API and CLI) and **7481** (nodes).
Allow both through the firewall from your workstations only.

> **Keep both ports on a private network or a tailnet, never the open
> internet.** They are authenticated and encrypted, but Switchworks is not
> hardened for internet exposure. To reach the web UI from elsewhere, use a
> VPN or `tailscale serve` (see [docs/web-ui.md](docs/web-ui.md)).

### 2. Each workstation

Install and sign in to the harnesses you want to use there (see
[Harnesses](#harnesses)). Then enroll the machine with a one-time token
from the coordinator. The token carries the coordinator's address and CA
fingerprint, so there is nothing else to copy:

```bash
# on the coordinator
swk coordinator enroll-token devbox
# on the workstation: paste the token it printed (read from stdin, so it
# never appears in the process list)
swk node enroll -
```

or in one step over SSH:

```bash
ssh coord.lan swk coordinator enroll-token devbox | swk node enroll -
```

The host name you give the token (`devbox`) is how the fleet names that
machine. Then run the node as your user, at login and now:

```bash
swk service install              # the node; `swk service status` shows how it is doing
```

| OS | What `swk service install` sets up |
| --- | --- |
| Linux | The `swk-node` systemd user unit: the packaged one, or one it writes to `~/.config/systemd/user`. Run `loginctl enable-linger "$USER"` to keep it running while you are logged out. Logs: `journalctl --user -u swk-node` |
| macOS | A LaunchAgent, `~/Library/LaunchAgents/dev.switchworks.node.plist`, with your login shell's `PATH` so your harnesses resolve (run the install again after changing it) |
| Windows | A Task Scheduler logon task that runs `swkw.exe node run` in your session, with least privilege, so it can use your sign-ins |

`swk service start|stop|restart|status|uninstall` manage it; the tray icon does the same.
Or run it in a terminal instead:

```bash
swk node run
```

`swk hosts` on the coordinator should now show the machine as online,
with the harnesses it found.

`swk node enroll` checks the coordinator's CA against the fingerprint in
the token, so a mismatch fails there. To compare by hand, hash the
certificate itself rather than its PEM file: `openssl x509 -in ca.crt
-noout -fingerprint -sha256` on Linux and macOS, or in PowerShell
`(New-Object Security.Cryptography.X509Certificates.X509Certificate2 ca.crt).GetCertHashString('SHA256')`
(`certutil -hashfile` hashes the PEM text, which never matches).

> **macOS note:** macOS's Local Network privacy setting blocks LAN
> connections from programs it has not approved, and `swk node enroll` or
> `swk node run` then fails with `connect: no route to host` even though
> `ping` and `curl` work. Allow the terminal app (or Switchworks) under
> System Settings, Privacy & Security, Local Network. A
> long-lived tmux server can keep the denial after you allow the terminal;
> start the node from a new terminal window instead.

### 3. The CLI on other machines

The CLI works anywhere that can reach port 7480. Each machine gets its own
named API token with only the access it needs. On a machine that has the
admin token (the coordinator, or wherever you keep it), make a complete
`cli.json` for another one:

```bash
swk token create devbox --scope operate --cli-config > devbox-cli.json
# copy it to devbox as ~/.config/swk/cli.json (chmod 600)
```

| Scope | What it allows |
| --- | --- |
| `read` | Look only: hosts, sessions, transcripts, live streams, messages, usage. `swk ui-link` (read-only links). |
| `operate` | `read`, plus day-to-day control: send, ask and reply, approve or decline, interrupt, stop, and spawn sessions. |
| `admin` | Everything, including enrollment tokens, `swk hosts rm` and managing tokens. The token `init` made is admin. |

**Put only `read` or `operate` tokens on machines where agents run.**
Harness sandboxes limit writes, not reads, so an agent there can read any
`cli.json` on the machine. With an operate token it can do what you can do
from that CLI, but cannot enroll hosts, revoke them or make tokens, and you
can cut it off with one command. Note that `operate` includes spawning:
sessions you spawn run with the options you give them, outside the guards
that confine agents' own spawns. Use `read` where that is too much.

```bash
swk token ls                      # name, scope, last use, expiry; never the secret
swk token rm devbox               # revoke it; browsers it signed in are signed out
swk token create phone --scope read --ttl 720h    # prints just the secret, once
```

Keep the admin token itself in a password manager or on one trusted
machine without agents. `swk coordinator cli-config` prints the admin
`cli.json`; use it only there. `SWK_CLI_CONFIG=/path/to/cli.json swk ...`
picks a config for one command.

### 4. The tray icon

`swk-tray` (Linux and Windows) and the Switchworks menu-bar app (macOS) show whether this
machine's node is connected, and whether a coordinator runs here. Their menu opens the web UI,
starts, stops or restarts the node and coordinator services (or sets them up), opens the logs
and config folders, and can start the tray at login. They only read local status files and call
`swk service`; they open no ports.

- **Linux**: start **Switchworks** from your applications menu, or run `swk-tray`. It is a
  StatusNotifierItem, which KDE Plasma, Xfce, Cinnamon, LXQt and others show natively; GNOME
  needs the AppIndicator extension (`gnome-shell-extension-appindicator`).
- **Windows**: the installer starts it, and at sign-in if you keep that option.
- **macOS**: open Switchworks from Applications; it has no Dock icon.

### 5. The web UI

Open `https://coord.lan:7480/`. Because the certificate comes from the
fleet's own CA, the browser warns at first. Either:

- install the CA: open `https://coord.lan:7480/ca.crt`, check its SHA-256
  fingerprint against the one `swk coordinator init` printed, and trust it (on
  iOS: Settings, General, About, Certificate Trust Settings), or
- put the UI behind `tailscale serve` for a publicly trusted certificate
  (see [docs/web-ui.md](docs/web-ui.md)).

Sign in with a token (a `read` or `operate` one for a browser you use
day to day), or run `swk ui-link` anywhere the CLI works and open the
one-time link it prints. That is the easy way to sign in on a phone. The
browser gets the scope of the token that signed it in, or of the token that
made the link (`swk ui-link --scope read` makes a read-only one).

---

## Usage

### Your first session

In the web UI, describe a task in the box at the bottom of the home screen, pick the host,
harness and folder, and send. Or from the CLI:

```bash
swk spawn devbox --cwd ~/src/myapp --name myapp "run the tests and fix what fails"
swk attach devbox/myapp          # follow it live; Ctrl-C detaches
```

### Everyday commands

```bash
swk ls                                    # every session on every host
swk send myapp "also update the changelog"           # next turn
swk send --steer myapp "skip the docs for now"       # into the running turn (Codex)
swk send --image shot.png myapp "does this look right?"    # with an image (up to 4)
swk approve myapp <approval-id> accept    # or answer in the web UI
swk interrupt myapp
swk stop myapp                            # history is kept
swk rm myapp                              # delete a stopped session's history (admin)
```

A bare session name works when it is unique across the fleet.

Stopped sessions, and sessions whose host no longer has them, are deleted with their event logs
90 days after their last event. Set `session_retention_days` in `coordinator.json` to change
that (a negative number keeps them forever), or remove one sooner with `swk rm`. Sessions that
are still running, or in error on their node, are kept however old.

Agents get fleet tools (`fleet_list`, `fleet_ask`, `fleet_send`,
`fleet_spawn`, ...), so a prompt like "ask buildbox/helper whether the ARM
build passes" makes one agent ask another on a different machine. You can follow and join in:

```bash
swk messages                  # who said what to whom
swk inbox                     # questions agents asked you
swk reply <ask-id> "ship it"
swk ask helper "status?"      # ask an agent yourself and wait for the answer
```

Guards in `coordinator.json` bound what agents can do to each other: hop
limits, rate limits, spawn depth and a fleet-wide daily token budget
(`guards.max_daily_tokens`; set one before letting agents spawn freely). By
default an agent spawns only on its own host, with its own harness, inside
its own working directory; its children inherit its approval policy and
sandbox, and a sandboxed agent cannot start an unsandboxed harness (ACP
agents such as goose ignore sandboxes). Agents steer, interrupt and stop only sessions they spawned. See
[docs/messaging.md](docs/messaging.md), including its trust model: the
guards are not a sandbox, and an agent with a shell can read its node's
credentials.

Messages and first prompts can carry images (PNG, JPEG, WebP or GIF, up to
4 per message, 5 MiB each): `--image` on `swk send`, `msg`, `ask` and
`spawn`, the attach button or a paste in the web UI, and an `images` list of
file paths in `fleet_send`, `fleet_ask` and `fleet_spawn`, so one agent can
hand a screenshot to a reviewer on another host. Agents can attach only
regular files inside their own working directory. Codex and Goose take
images; whether the model actually sees them depends on the model and the
provider endpoint, which `swk dev vision-probe` checks. See
[docs/messaging.md](docs/messaging.md#images).

If a workstation is lost or compromised, `swk hosts rm <host>` revokes its
certificate and `swk token rm <name>` its CLI token. Enrolling a host
again also replaces its old certificate.

The coordinator pins each host's node certificate when it enrolls, and
refuses any other certificate for that name. Hosts enrolled before
pinning have no pin yet: the first certificate the fleet CA signed for
that name to connect is pinned, which could be an older certificate for
the same name rather than the current one. The coordinator logs a
`WARNING` for each such host when it starts; enroll those hosts again to
pin their certificates now. (Refusing them instead would have cut every
existing host off at the upgrade.)

### Harnesses

Each node offers the harnesses it finds on that machine. Configure them in
the node's `node.json` (in `~/.config/swk/node/` on Linux,
`~/Library/Application Support/swk/node/` on macOS,
`%AppData%\swk\node\` on Windows); restart the node after editing
(`swk service restart`).

- **Codex**: `codex` on `PATH` and signed in, or with providers configured
  in its `config.toml`. Codex only accepts providers that speak the OpenAI
  Responses API; DeepSeek and MiMo do. On Windows, `winget install
  OpenAI.Codex` may not create the `codex` command alias; add it if
  `codex` is not found.
- **Goose**: `goose` on `PATH` with a provider configured (`goose
  configure`, or environment variables in `node.json`). DeepSeek, GLM and
  MiMo work directly. Goose does not run on Windows on ARM.
- **Other ACP agents**: add them by name, for example
  `"acp": {"opencode": {"bin": "opencode", "args": ["acp"]}}`.
- **agy** (experimental): `agy` on `PATH`, signed in once interactively.
  Headless agy cannot ask for approval: the approval policy picks its
  mode, and commands and fleet tools need allow rules in its settings
  unless the session runs with approval `never` or sandbox
  `danger-full-access`. See [docs/harnesses.md](docs/harnesses.md#agy-antigravity-cli).

[docs/harnesses.md](docs/harnesses.md) covers each one: approvals, models
and providers, fleet tools, and what each supports.

`node.json` also sets spawn defaults (`default_approval_policy`,
`default_sandbox`) and the harness binaries (`codex.bin`, `agy.bin`, ...).

### Trying it without spending tokens

`swk dev mock-provider` serves a fake model that speaks both the OpenAI
Responses API and the Anthropic Messages API, scripted by the prompt:

```bash
swk dev mock-provider --codex-home /tmp/mock-home &
swk dev run --codex-home /tmp/mock-home --approval untrusted "RUN: touch x"
```

`swk dev run` drives one turn of any harness directly, without a node or
coordinator: a quick check that a harness works on a machine. With
`--record FILE` it captures the raw protocol stream, in a file only you
can read (0600; on Windows the file takes its folder's permissions, so
keep it in your profile). Like the fleet commands, it strips terminal escapes and
invisible characters from what agents and harnesses print, so a command
in an approval prompt shows as it would run.

`swk dev vision-probe [--harness goose] [--model M]` sends the harness an
image with a random code and reports PASS if the model reads it back: run
it on each host with your real configuration to see which models can see
images. Against the mock provider it checks the image path without a model.

---

## Development

```bash
make test        # vet, then unit and in-process fleet tests with the race detector
make test-codex  # also the real-Codex integration test (needs codex on PATH; no tokens spent)
make cross       # check that every release target still builds
```

The real-Goose tests run whenever `goose` is on `PATH` (or `SWK_GOOSE_BIN` is set), against the
built-in mock provider. The protocols are described in
[`docs/codex-app-server-notes.md`](docs/codex-app-server-notes.md),
[`docs/messaging.md`](docs/messaging.md), [`docs/harnesses.md`](docs/harnesses.md) and
[`docs/web-ui.md`](docs/web-ui.md).

Switchworks uses only the Go standard library, the tray included: the Linux tray speaks D-Bus
through a small client in `internal/dbus`, and the Windows tray calls the Win32 API directly. The
macOS menu-bar app is a short Swift program built with `swiftc`.

## Project Structure

```text
cmd/swk/                 # The swk binary
cmd/swk-tray/            # Tray icon for Linux and Windows
macos/Switchworks/         # macOS menu-bar app (Swift)
internal/
├── coord/                # Node listener, event store, message router, HTTPS API and SSE
├── node/                 # Sessions, spool, delivery queue, coordinator link, local API
├── adapter/
│   ├── codexad/          # Codex (codex app-server)
│   ├── acpad/            # Goose and other ACP agents
│   └── agyad/            # Antigravity agy (headless stream-json)
├── harness/              # Harness registry and node.json harness config
├── codex/                # app-server JSON-RPC client, contract-tested against schema/codex
├── acp/                  # ACP client; acptest is a scripted fake agent
├── jsonrpc/              # Shared JSON-RPC 2.0 over stdio
├── mcp/                  # swk mcp: the fleet tools as a stdio MCP server
├── fleet/                # Fleet operations: arguments and results
├── attach/               # Image attachments: type sniffing, limits, confined file reads
├── web/                  # Web UI (embedded HTML, CSS, JS and fonts)
├── store/                # Storage interface and file-backed implementation
├── pki/                  # Private CA and enrollment tokens
├── wire/                 # Node ↔ coordinator protocol
├── cli/, apiclient/, render/  # The swk command line
├── devrun/               # swk dev run, vision-probe and mock-provider
├── service/              # swk service: systemd user units, LaunchAgents, logon tasks
├── runstatus/            # status.json, which the tray and swk service status read
├── tray/                 # Tray menu and actions; sni/ (Linux D-Bus), wintray/ (Win32)
├── dbus/                 # Minimal D-Bus client for the Linux tray
└── e2e/                  # In-process multi-host fleet tests
schema/codex/             # codex app-server generate-json-schema --experimental output
build-aux/                # Packaging: rpm, deb, Arch, Inno Setup, winget, macOS app
data/                     # Desktop entry and icons
docs/                     # Design notes, protocols and status
```

## Scope

Switchworks is a control plane, not an agent or a model gateway. It does not edit code itself,
proxy model traffic, or store provider keys beyond what you put in a node's `node.json`. It is
built for one person's (or one small team's) machines behind a private network, not as a
multi-tenant service.

---

## Security

Report vulnerabilities privately, as described in [`SECURITY.md`](SECURITY.md), not in a public
issue. It also sets out what Switchworks assumes about your network and your agents.

---

## License

Switchworks is licensed under the [GNU General Public License v3.0 or later](LICENSE).

The web UI embeds the Onest and JetBrains Mono fonts, licensed under the SIL
Open Font License 1.1; see `internal/web/assets/fonts`.

`schema/codex/` and `internal/codex/testdata/` contain output generated by
[openai/codex](https://github.com/openai/codex) (Apache-2.0); see
[schema/codex/README.md](schema/codex/README.md).
