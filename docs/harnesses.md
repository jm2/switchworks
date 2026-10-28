# Harnesses: Codex, Goose and other ACP agents, agy

Switchworks manages harnesses; it is not one. Each node runs whichever of
these it finds, and `swk hosts` (and the spawn form, and `fleet_list`)
shows what each host offers. Spawn with `--harness <name>`.

| Harness | Protocol | Approvals relayed | Steer | Fleet tools | Images | Resume after node restart | Status |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `codex` | `codex app-server` (JSON-RPC, stdio) | yes | yes | per thread (MCP) | yes (`localImage`) | `thread/resume` | done |
| `goose` | ACP over stdio (`goose acp`) | yes | no (queued) | per session (ACP `mcpServers`) | yes (ACP image blocks) | `session/load` | done; tested against goose 1.54.0 and 1.55.0 |
| other ACP agents | ACP over stdio | yes | no (queued) | per session | if advertised | `session/load` or `session/resume` | experimental; tested with a fake ACP agent |
| `agy` | headless `stream-json` | no (agy never asks) | no (queued) | opt-in, via workspace config | no | `--conversation` | experimental; run against agy 1.4.0 |

Steering is a Codex feature. For the others, a message sent to a running
turn (the default mode, `auto`, or `--steer`) is queued for the next turn,
together with any other waiting messages.

## Turns cut off by a restart

A turn running when the node goes down (an upgrade, a reboot, a crash)
does not survive it: Codex records it as interrupted, and the resumed
session would sit idle until someone prompted it. The node saves each
session's running turn in `state.json` as it starts, and clears it as the
turn ends or as a person interrupts it (Stop in the web UI, `swk
interrupt`, a message sent with `interrupt`). A turn that ends because the
node is shutting down stays saved. When the node starts again and has
resumed such a session, it queues one `system` message from `swk`, which
starts a new turn:

```
[switchworks msg=01M40… from=swk kind=system]
Your previous turn (id 019a…) was cut off when Switchworks restarted at 2026-10-28 03:12 UTC. Continue where you left off; check the state of anything you were running.
```

The same goes for a turn cut off while the node keeps running, when
Codex's app-server crashes under it: the adapter restarts app-server and
resumes the thread, and the message then says the turn "was cut off when
Codex restarted". It goes once per interruption, together with any
messages that waited in the queue, and never to a stopped session or one
whose harness still runs the turn. Set `"continue_interrupted": false` in `node.json` to turn it
off. A node upgraded from a version before 0.1.7 has no saved turn yet, so
a turn its restart for the upgrade cut off is not continued.

## Models without a translation proxy

DeepSeek and Xiaomi MiMo work in both Codex and goose without a proxy.
The configurations below were run in the 2026-10-16 fleet test with
DeepSeek V4.1 Flash (`deepseek-flash`) and MiMo V2.6 Pro (`mimo-v2.6-pro`),
on Linux, macOS and Windows.

**Codex** only speaks the OpenAI Responses API, which both providers now
serve. In Codex's `config.toml` (top-level keys before any table):

```toml
model = "deepseek-flash"
model_provider = "deepseek"
web_search = "disabled"   # MiMo rejects Codex's web_search tool

[model_providers.deepseek]
name = "DeepSeek"
base_url = "https://api.deepseek.com"
wire_api = "responses"
experimental_bearer_token = "…"

[model_providers.mimo]
name = "MiMo"
base_url = "https://api.xiaomimimo.com/v1"
wire_api = "responses"
experimental_bearer_token = "…"
```

Pick one per session with `swk spawn … --provider mimo --model
mimo-v2.6-pro`. Without `web_search = "disabled"`, every MiMo turn fails
with `responses_feature_not_supported: tool type 'web_search' is not
supported`. Codex reads `config.toml` when its app-server starts, so
restart the node after editing it.

**Goose** reaches both through OpenAI-compatible providers. DeepSeek is
built into goose 1.55 as `custom_deepseek`; MiMo needs a declarative
provider in `~/.config/goose/custom_providers/custom_mimo.json`
(`%APPDATA%\Block\goose\config\custom_providers\` on Windows):

```json
{
  "name": "custom_mimo",
  "engine": "openai",
  "display_name": "MiMo",
  "api_key_env": "CUSTOM_MIMO_API_KEY",
  "base_url": "https://api.xiaomimimo.com/v1",
  "models": [{"name": "mimo-v2.6-pro", "context_limit": 128000, "supports_vision": true}],
  "supports_streaming": true
}
```

Keys go in goose's keyring or, with `GOOSE_DISABLE_KEYRING: true` in
`config.yaml` (headless machines), in `secrets.yaml` under
`DEEPSEEK_API_KEY` and `CUSTOM_MIMO_API_KEY`. Spawn with `--provider
custom_deepseek --model deepseek-flash` or `--provider custom_mimo --model
mimo-v2.6-pro`. Without `"supports_vision": true`, goose 1.55 drops
images for that model before they reach the provider (the model then says
it cannot see them).

Goose's `anthropic` provider is an alternative, since all three providers
also speak Anthropic Messages:

| Provider | Anthropic-compatible host (`ANTHROPIC_HOST`) |
| --- | --- |
| DeepSeek | `https://api.deepseek.com/anthropic` |
| Z.ai GLM | `https://api.z.ai/api/anthropic` |
| Xiaomi MiMo | `https://api.xiaomimimo.com/anthropic` |

Each `acp` entry in `node.json` is its own harness name, so one host can
offer several goose configurations side by side:

```json
"acp": {
  "goose": {
    "env": {"GOOSE_PROVIDER": "anthropic", "ANTHROPIC_HOST": "https://api.z.ai/api/anthropic",
            "ANTHROPIC_API_KEY": "…", "GOOSE_MODEL": "glm-5.3"}
  },
  "goose-deepseek": {
    "bin": "goose", "args": ["acp"],
    "env": {"GOOSE_PROVIDER": "anthropic", "ANTHROPIC_HOST": "https://api.deepseek.com/anthropic",
            "ANTHROPIC_API_KEY": "…", "GOOSE_MODEL": "deepseek-flash"}
  }
}
```

`swk spawn devbox --harness goose-deepseek …` then runs that one. The
Anthropic endpoints have been exercised only through the mock provider's
Anthropic Messages emulation (`swk dev mock-provider`), not with real keys.
Keys in `node.json` are plain text (0600).

## Codex: node.json settings

The `codex` object in `node.json` configures how the node runs
`codex app-server`. Every key is optional:

| Key | What it does |
| --- | --- |
| `bin` | The `codex` binary (default `codex` on `PATH`). |
| `home` | `CODEX_HOME` for app-server (default: Codex's own, `~/.codex`). |
| `args` | Codex's **whole** argument list, subcommand included. Default `["app-server"]`. |
| `config` | Config overrides, passed as `-c key=value` ahead of `args`. |
| `lean` | `true` adds the [lean defaults](#codex-lean-sessions) to `config`. Default `false`. |
| `permissions` | A permission profile for `workspace-write` sessions ([below](#codex-network-without-exposing-credentials)). |
| `session_env` | Variables and `PATH` directories for the commands sessions run ([below](#codex-session-environment)). |

`args` replaces the argument list rather than adding to it, so it must
still name the subcommand: `["-c", "x=1"]` alone runs Codex without
`app-server`, and every session fails with "app-server initialize:
jsonrpc: connection closed". The node refuses to start with such an
`args`. Put overrides in `config` instead:

```json
"codex": {
  "config": {
    "model_auto_compact_token_limit": 128000,
    "web_search": "disabled",
    "features": {"apps": false}
  }
}
```

The node passes these as `-c features.apps=false -c
model_auto_compact_token_limit=128000 -c web_search="disabled"
app-server`. Values are written in TOML: strings quoted and escaped,
numbers and booleans as they are, arrays as arrays. A nested object
becomes dotted keys, so `"features": {"apps": false}` and
`"features.apps": false` are the same. Each part of a key must be
letters, digits, `_` or `-`, since Codex splits keys on dots. A key set
both in `config` and by a `-c` in `args` stops the node at start, as
does a `null` value. Overrides apply to every Codex session on the node
and take precedence over `config.toml`; restart the node after changing
them.

A turn carries the session's approval policy (`turn/start`'s
`approvalPolicy`) only when it differs from the thread's: after a policy
change (`swk policy`, the web UI's Approvals menu), not on every turn.
app-server applies any `turn/start` override as a thread settings update
and records a `thread_settings_applied` event for it; with the same policy
each time, that was one per turn. `thread/start` and `thread/resume` still
carry the policy, the sandbox (or permission profile) and the fleet tools'
MCP config, since the node checks the sandbox the thread reports and a
restarted node gives the tools a new address and credential; Codex records
its settings once at every resume whatever it is sent.

When app-server fails to start, the node log has the end of what it
wrote to stderr, which usually says why (an unknown argument, a
`config.toml` error). The full output is in `logs/harness.log` in the
node directory.

### Codex: session environment

`codex.session_env` sets variables for the commands Codex sessions run,
and adds directories to their `PATH`:

```json
"codex": {
  "session_env": {
    "set": {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.sslBackend", "GIT_CONFIG_VALUE_0": "openssl"},
    "path_prepend": ["C:\\Users\\thesa\\tools\\glslang"],
    "path_append": []
  }
}
```

The first example makes Git for Windows use OpenSSL instead of
Windows' TLS stack, as `git -c http.sslBackend=openssl` would, for every
`git` a session runs. The second puts a directory first on `PATH`, here
for an arm64 `glslangValidator` on a Windows arm64 host, so the agent
finds it before any other.

The node applies `session_env` to the environment it starts
`codex app-server` with, and Codex passes its environment on to every
command (its default `shell_environment_policy.inherit = "all"`). Codex
then adds its own `PATH` entries on top: its `apply_patch` shim, the
Windows sandbox's helpers, its versioned `codex-path` folder. That is
why `PATH` doesn't belong in `shell_environment_policy.set`, which
would freeze a copy of those entries and break at the next Codex update.

- `set` replaces a variable's value. A `PATH` in it replaces the node's
  `PATH`, and `path_prepend` and `path_append` then apply to it.
- `path_prepend` and `path_append` add directories ahead of and after the
  node's `PATH`: on Linux and macOS that's the login shell's `PATH` that
  `swk service install` gives the service (the `swk-path.conf` drop-in on
  Linux). Each directory must be absolute (`~/` is your home) and appears
  once: a prepended one already on `PATH` moves to the front, an
  appended one already there stays where it is. The node joins them with the OS's
  separator (`;` on Windows, `:` elsewhere) and keeps the node's spelling
  of the name (`Path` on Windows).
- A `shell_environment_policy.set.<VAR>` in `codex.config` (or a `-c` in
  `codex.args`, or `config.toml`) wins over `session_env` for that
  variable, since Codex applies it after inheriting.
- `shell_environment_policy.inherit = "core"` keeps only a core set of
  variables (`PATH`, `HOME` and a few others), and `"none"` drops all of
  them, `session_env` included. With `ignore_default_excludes = false`,
  Codex also drops every variable whose name contains `KEY`, `SECRET` or
  `TOKEN`, so `GIT_CONFIG_KEY_0` from the example; set such ones in
  `shell_environment_policy.set` instead.
- The variables are app-server's own too, so they also reach Codex's
  own `git` calls and MCP servers. `CODEX_HOME` can't be set here: use
  `codex.home`.

Restart the node after changing `session_env`.

### Codex: session caches

Inside the sandbox `~/.cache` isn't writable, so `ccache` (with
`USE_CCACHE=1`, or Fedora's `/usr/lib64/ccache` first on `PATH`) and
`gh` (`~/.cache/gh`) fail. The node gives each Codex session a cache
directory of its own, `<user cache dir>/switchworks/session-cache/<session id>`
(`~/.cache/...` on Linux, `~/Library/Caches/...` on macOS,
`%LOCALAPPDATA%\switchworks\...` on Windows; mode 0700), and points
`XDG_CACHE_HOME` and `CCACHE_DIR` at it in that session's commands.

The directory is made writable for that session's thread only, through
the per-thread config Switchworks sends with `thread/start` and
`thread/resume`, so one agent can't write, or poison, another's cache:

- **Permission profile** (`codex.permissions`, on Linux and macOS): a
  `"<dir>" = "write"` entry is added to the profile's filesystem table.
  It merges with your `[permissions.<name>.filesystem]` table in
  `config.toml`. If `codex.config` or `codex.args` sets that table (or
  the whole profile) with `-c`, the entry would replace it, so the node
  logs that it can't add one and the directory stays read-only: add
  the cache root to the profile yourself in that case.
- **Legacy `workspace-write`** (Windows, and any node without
  `codex.permissions`): the directory is added to
  `sandbox_workspace_write.writable_roots`. The per-thread value replaces
  the whole list, so the node sends it with the roots you set: those in
  `codex.config` or `codex.args`, else those in `config.toml` (in
  `codex.home`, `CODEX_HOME` or `~/.codex`; a project's
  `.codex/config.toml` is not read, and its `writable_roots` don't apply
  to managed sessions). Write them as an array of strings, in a
  `[sandbox_workspace_write]` table or as
  `sandbox_workspace_write.writable_roots`. In another form (an inline
  table, say) the node can't read them, logs so at start, and leaves the
  list alone; the directory then stays read-only.
- `danger-full-access` sessions get the variables, and `read-only`
  sessions get nothing.

Either way, a variable you set yourself is left alone: `CCACHE_DIR` or
`XDG_CACHE_HOME` in `codex.session_env.set`, in
`shell_environment_policy.set` in `codex.config` or `codex.args`, or in
`[shell_environment_policy.set]` in `config.toml`. When the node can't
make the directory writable it still sets the variables, so tools fail
as they would have on `~/.cache`.

The directory is kept across node restarts and removed when the session
stops, fails to start, or is forgotten; at start the node also removes
those of sessions it no longer has. `session_cache` in `node.json`
moves the root (`{"dir": "/srv/swk-cache"}`) or turns it off
(`{"disabled": true}`). Keep the root out of `/tmp` and `$TMPDIR`,
which every legacy `workspace-write` session can write, and out of any
session's working directory; and keep it out of the paths your profile
denies (`~/.config/swk`). Another session whose working directory
contains the root, such as one in your home directory, can still write
it.

Verified with Codex 0.163 on Linux, legacy and profile
(`SWK_CODEX_IT=1 go test -run TestRealCodexSessionCache ./internal/e2e/`).
On macOS and Windows the overrides are the same; that the sandboxes
there honor them is not yet checked on a live host.

### Codex: lean sessions

A managed Codex session pays for every tool Codex offers on every model
call. On 2026-10-21, the first call of a fresh thread sent 47.5K input
tokens with Codex's defaults, and 12.8K with apps (the ChatGPT GitHub
connector, about 30 tools), the browser, computer use, image generation,
plugins and web search turned off: about 35K tokens less on each of
thousands of calls a day. For a model Codex doesn't know, such as
`deepseek-flash`, Codex uses fallback metadata, a 258,400-token window
that compacts at about 245K, so average calls carried 140K to 170K
tokens.

`"codex": {"lean": true}` is recommended for managed sessions. It adds
these overrides:

```json
{
  "features": {
    "apps": false, "browser_use": false, "browser_use_external": false,
    "computer_use": false, "image_generation": false, "plugins": false,
    "in_app_browser": false
  },
  "web_search": "disabled",
  "tool_output_token_limit": 1500,
  "model_auto_compact_token_limit": 128000
}
```

Turning apps off also matters for security. The GitHub connector lets
an agent act on GitHub as the owner, around any agent-specific token you
give it.

Each override is added only where `codex.config`, or a `-c` in
`codex.args`, doesn't set the same key. To keep web search, for example,
set `"web_search": "live"` in `codex.config`. The overrides take
precedence over `config.toml`, so set a key in `codex.config` to keep the
`config.toml` value. `lean` never sets the reasoning effort. For a model
Codex doesn't know, also set its real window in `codex.config`
(`"model_context_window": 128000`, say), since Codex can't compact
correctly without it.

### Codex on Windows

Codex 0.163 can't give a thread `workspace-write` on Windows unless a
Windows sandbox mode is set, and runs it read-only without saying so.
Under approval `never`, every command then needs escalation and is
rejected ("… rejected: blocked by policy"), even `git --version`. So on
Windows the node passes `-c windows.sandbox="unelevated"` (no
administrator setup needed) unless a mode is already set: `sandbox` in a
`[windows]` table or a top-level `windows.sandbox` in `config.toml` (in
`codex.home`, `CODEX_HOME` or `~/.codex`), or `windows.sandbox` in
`codex.config` or `codex.args`. The node log says when it passes it. The
node only checks whether `config.toml` sets the key and never logs the
file, which holds provider keys. A permission profile with deny rules
needs the elevated sandbox, so set `"windows": {"sandbox": "elevated"}`
in `codex.config`, or `[windows] sandbox = "elevated"` in `config.toml`.

After every thread start and resume, the node compares the sandbox the
thread reports with the one the session asked for. A thread that runs
with a looser sandbox than asked for (`danger-full-access` or
`externalSandbox` for a `workspace-write` session, say) is refused: the
spawn or resume fails, and the node log names the thread. Under a
permission profile, `externalSandbox` is how Codex reports a profile its
older sandbox types cannot describe, so it is accepted there. When Codex
reports no sandbox, or one the node does not know, a session an agent
spawned is refused the same way, and a person's session gets a warning.
When the thread runs with a stricter sandbox than asked for, the
session gets a warning event and a status detail starting "degraded:",
and the node log names the thread. The web UI shows both in the session
log. `caps.read_only` in `fleet_list` and `fleet_status` follows the
sandbox the thread reports, and the session's details in the web UI say
"(runs read-only)" when it does. Codex may not report every downgrade,
so a session that can't write or run commands can still look healthy.

Codex runs commands with `pwsh` when it's on `PATH`. If the only `pwsh`
is the Microsoft Store version (an App Execution Alias in
`%LOCALAPPDATA%\Microsoft\WindowsApps`), the sandbox's restricted token
can't start it ("CreateProcessAsUserW failed: 5"), and every command
fails. The node warns about this at start. Install PowerShell 7 from the
MSI (`winget install --id Microsoft.PowerShell --source winget`, or the
MSI from the PowerShell releases), then restart the node.

## Codex: network without exposing credentials

Codex's `workspace-write` sandbox limits writes and, by default, the
network. It does not limit reads: a sandboxed command can read your home
directory, including Codex's `config.toml` (provider keys), `~/.ssh`, the
GitHub CLI's token and Switchworks's `cli.json` and node key.
`[sandbox_workspace_write] network_access = true` turns the network on
with all of that readable, so one prompt-injected command can send any of
it out. Don't use it.

A Codex permission profile can turn the network on and deny reads of
chosen paths. Define one in `config.toml` (Codex requires
`default_permissions` once a profile exists):

```toml
default_permissions = "swk"  # before any table

[permissions.swk]
extends = ":workspace"

[permissions.swk.network]
enabled = true

[permissions.swk.filesystem]
"~/.codex/config.toml" = "deny"
"~/.codex/auth.json" = "deny"
"~/.codex/history.jsonl" = "deny"
"~/.codex/sessions" = "deny"
"~/.codex/archived_sessions" = "deny"
"~/.config/gh" = "deny"
"~/.config/goose" = "deny"
"~/.config/swk" = "deny"
"~/.ssh" = "deny"
"~/.claude" = "deny"
"~/.claude.json" = "deny"
"~/.gemini" = "deny"
"~/.gnupg" = "deny"
```

That short list is not enough on its own. Deny every credential store on
the host: other agents' logins (Claude Code's `~/.claude` and
`~/.claude.json`, agy's `~/.gemini` and `~/.antigravity`), package and
cloud tokens (`~/.config/copr`, `~/.npmrc`, `~/.pypirc`,
`~/.git-credentials`, `~/.netrc`, `~/.cargo/credentials.toml`, `~/.aws`,
`~/.azure`, `~/.config/gcloud`, `~/.kube`, `~/.docker`), GnuPG and
`~/.password-store`, the keyring (`~/.local/share/keyrings`), and browser
profiles, which hold signed-in cookies. On Linux and macOS a deny path
that doesn't exist yet is fine, so list the ones a later login would
create too. Codex's own transcripts (`sessions`, `history.jsonl`) can
hold pasted secrets. Don't deny `~/.codex` itself: the Codex binary and
its helpers live there.

Those are the Linux paths. On macOS, deny
`~/Library/Application Support/swk` in place of `~/.config/swk`, and the
apps' folders under `~/Library/Application Support` (Claude, Antigravity,
Code, browsers) plus `~/Library/Cookies`.

Leave the directory of received files readable: `~/.cache/switchworks`
(`~/Library/Caches/switchworks` on macOS, `~/AppData/Local/switchworks`
on Windows), where the node writes the files messages carry and from
where agents read them ([messaging.md](messaging.md#files)). It holds
nothing but those files, and is outside `~/.config/swk` for that reason.
Under Windows' elevated sandbox the sandbox accounts may not be able to
read your profile's `AppData\Local`; this has not been tried, so set
`files_dir` in `node.json` to a folder they can read if a sandboxed
agent there cannot open its files.

The fleet tools read the files agents attach as you, outside the
sandbox, so the node also refuses attachments from every path a profile
in `config.toml` denies (in the `"<path>" = "deny"` form above), besides
its own list of credential locations and `attach_deny` in `node.json`
([messaging.md](messaging.md#images)). It does not read inline tables,
so keep the filesystem table in the form shown.

Then name the profile in `node.json`:

```json
"codex": {"permissions": "swk"}
```

Codex ignores profiles for a thread given a sandbox mode, which is how
Switchworks starts and resumes sessions. With `codex.permissions` set,
`workspace-write` sessions run under that profile instead; `read-only`
and `danger-full-access` sessions are unchanged. A profile `config.toml`
does not define fails the spawn or resume ("refers to undefined
profile"), so define it before setting `codex.permissions` and restarting
the node. Restart the node after changing the list, so running sessions
pick it up.

Check a profile from inside the sandbox, and check a file inside each
denied directory as well as the directory:
`codex sandbox -- sh -c 'head -c1 ~/.claude.json >/dev/null 2>&1 && echo READABLE'`
should print nothing. Through Switchworks,
`swk dev run --codex-permissions swk "Run ls ~/.ssh and say whether it worked"`.

Known limits:

- **Linux needs Codex 0.163 or later.** Before it, bubblewrap fails when a
  profile denies more than one file ("Can't write data to file … Bad
  file descriptor"). `codex update` upgrades a standalone install.
- **Windows: no sandbox network for now.** Deny rules need the elevated
  sandbox (`[windows] sandbox = "elevated"`, set up with
  `codex sandbox setup --elevated`); the unelevated sandbox, which the node
  picks when no mode is set ([Codex on Windows](#codex-on-windows)),
  refuses to run commands under a profile that has them. As of Codex 0.161 the elevated
  sandbox has two open bugs: HTTPS through Windows' TLS stack fails
  inside it ([openai/codex#17459](https://github.com/openai/codex/issues/17459)),
  and a running `node_repl.exe` from the desktop app's plugins makes
  every sandboxed command fail with "setup refresh had errors"
  ([openai/codex#48759](https://github.com/openai/codex/issues/48759)).
  Keep Windows nodes on the default sandbox without network and without
  `codex.permissions` until those are fixed. If you do use a profile
  there, a missing deny path is created as a folder, so list only paths
  that exist, and write `~/` and forward slashes
  (`~/AppData/Roaming/swk`, `~/AppData/Roaming/GitHub CLI`,
  `~/AppData/Roaming/Block/goose`).
- Codex before 0.161 may drop a profile's deny rules for a command you
  approve to run outside the sandbox.
- With the GitHub CLI's config and `~/.ssh` denied, `gh` and `git push`
  fail inside the sandbox. Approve pushes instead (approval
  `on-request`), or give the agents a token of their own, scoped to their
  repositories, in a path the profile leaves readable.

## Codex: other ways out of the sandbox

A deny list keeps a sandboxed command from *reading* secrets. It does not
stop the command from asking something outside the sandbox to act for it.
Close these too.

**Sockets (Linux).** Codex's bubblewrap sandbox keeps `/run/user/<uid>`
reachable, and a read-only mount doesn't stop a program from connecting to
a socket. Through the session bus, `systemd-run --user …` runs a command
as you, outside the sandbox. The real `/tmp` holds `/tmp/.X11-unix`, and
Xwayland lets your own user connect without a cookie, so an agent could
type into your terminals. Deny them, and turn the bus off for commands:

```toml
[permissions.swk.filesystem]
"/run/user/1000" = "deny"     # your uid; covers the bus, systemd, keyring, podman
"/tmp/.X11-unix" = "deny"
"/run/dbus" = "deny"          # the system bus
"/run/libvirt" = "deny"

[shell_environment_policy.set]
DBUS_SESSION_BUS_ADDRESS = "disabled:"
```

A deny inside a denied directory makes bubblewrap fail ("Can't create file
… Read-only file system"), so drop entries such as
`/run/user/1000/containers` when you deny `/run/user/1000`. Check from
inside the sandbox that
`systemd-run --user --wait touch /var/tmp/escape-test` fails and creates no
file. With the network on, the sandbox shares the host's network
namespace, so *abstract* Unix sockets (names starting with `@` in
`ss -xl`) stay reachable whatever the deny list says. Check that none of
them belongs to your desktop session (Xwayland listening on
`@/tmp/.X11-unix/X0`, for example); only turning the network off, or a
proxy-only network, closes that.

**Allow rules.** Codex's execpolicy rules in `$CODEX_HOME/rules/*.rules`
with `decision = "allow"` approve a matching command in advance. In a
session whose approval policy lets the agent ask to run a command outside
the sandbox (`on-request`, `untrusted`), the agent can then do that without
anyone being asked. (Under `never`, Codex refuses every such request.)
"Don't ask again" in interactive Codex writes such rules, and Switchworks
sessions share that CODEX_HOME unless `codex.home` gives them their own.
A rule like `["gh", "api"]` hands every agent your GitHub token. Keep
managed sessions' rules to `forbidden` ones (for example
`gh pr merge`, `git push origin main`), or give them a CODEX_HOME of their
own. Forbidden wins over allow for the same prefix, but a forbid on
`gh pr merge` doesn't stop `gh api …/merge`.

**Connectors.** A Codex app connector, such as the ChatGPT GitHub app, acts
with your account on the vendor's side, whatever token the sandbox has.
Turn apps off for managed sessions (`-c features.apps=false`).

**Git for agents.** Shell git inside the sandbox is unreliable: on
Windows `.git` is read-only and Git for Windows' `sh.exe` fails under the
restricted token, so even `git fetch` fails; on Linux the profile keeps
`.git/config` read-only, so `push -u`, `worktree add` and `branch -D`
half-fail. Agents should use the `workspace_git` tool instead (see
[Git for agents: workspace_git](#git-for-agents-workspace_git)). If you
also want shell git to work on Linux and macOS:
- `:workspace` keeps `.git` read-only. On Linux and macOS you can make it
  writable while keeping hooks and config out of reach:

  ```toml
  [permissions.swk.filesystem.":workspace_roots"]
  ".git" = "write"
  ".git/hooks" = "read"
  ".git/config" = "read"
  ".git/modules" = "read"
  ```

  Don't do this on Windows. There a read-only child of a writable folder
  can be deleted or replaced, so an agent could plant `core.hooksPath`,
  which then runs outside the sandbox on your next git command.
- For pushes from the shell, give the sandbox the same scoped token
  `workspace_git` uses (below), in a folder the profile leaves readable,
  and point `GH_CONFIG_DIR` and a git credential helper at it through
  `[shell_environment_policy.set]`. Check anything an agent hands off as a
  patch for secrets before pushing it (see below).

**Windows: the default sandbox can read everything you can.** Without
`[windows] sandbox = "elevated"`, Codex runs commands with your own,
restricted token. Deny rules only apply to the elevated sandbox's
separate accounts, so a sandboxed command can read the node's key
(`%APPDATA%\swk\node\node.key`), `~\.codex\auth.json` and `config.toml`
with its provider keys. Windows sandboxes have no network, but an agent can
still put what it reads into a message or a patch. Treat their output as
untrusted until the elevated sandbox works
([openai/codex#48759](https://github.com/openai/codex/issues/48759)).

<a id="gpu-access"></a>**GPU access.** The sandbox hides `/dev/dri`, so Vulkan and GL fall back to
llvmpipe, and a profile entry can't add it back. An allow rule doesn't help
either: under approval `never` Codex refuses every escalation. Instead,
run GPU work through a small service outside the sandbox that starts each
command in its own bubblewrap sandbox with `--dev-bind /dev/dri`, the
workspace writable, `.git` read-only, the profile's deny list hidden and
`--unshare-all` (no network, no sockets). Give the service a fixed
environment and apply the agent's environment only inside bubblewrap,
after `--clearenv`: a helper script that runs outside the sandbox with
the agent's `BASH_ENV`, `PATH` or `LD_PRELOAD` runs the agent's code as
you. Tests without a display can use SDL's offscreen driver, which still
renders on the real GPU.

**GPU access on Windows.** Codex's Windows sandbox, elevated or not, runs
commands on a private desktop (`CodexSandboxDesktop-*`). Direct3D 9 and
11 cannot reach the display from there (`D3DERR_NOTAVAILABLE`), though
Vulkan may work. Codex 0.156 removed the only way out of it
(`windows.sandbox_private_desktop = false`, [openai/codex#46554](https://github.com/openai/codex/pull/46554)).
Don't let agents work around it by starting programs on your desktop by
other means and driving them with simulated input: that leaves the
sandbox entirely. Instead, turn on the node's `gpu_run` tool:

```json
"gpu_run": {"enabled": true}
```

With it, an agent calls `gpu_run` with a command (`argv`, optional `cwd`,
`env` and `timeout_seconds`). The node, which runs in your logon session,
starts the command on your desktop (`winsta0\default`) and returns its
exit code and the last 64 KiB of stdout and stderr. The command is
confined:

- It runs in a Windows AppContainer, one per workspace, at low
  integrity. It can open only what is granted to that container or to
  all AppContainers: the session's working directory, its own folder
  (`%LOCALAPPDATA%\Packages\swk.gpu.*`, where `TEMP`, `APPDATA` and
  `LOCALAPPDATA` point), and the system files and `Program Files` that
  Windows lets every AppContainer read. Your profile, the node's key and
  `~\.codex` are out of reach. This is narrower than the default
  (unelevated) Codex sandbox, which reads everything you can.
- In the working directory, every `.git` is read-only (it can't be
  written, deleted, renamed or have its ACL changed), and the entries of
  the deny list inside it (the attachment deny list: credential
  locations, Codex profile denies, `attach_deny`) can't be opened at
  all. The node grants this with explicit ACEs that it sets on each file
  and directory itself, through a handle opened without following links.
  It never relies on Windows' own ACL inheritance, which follows paths
  and so could follow a junction out of the workspace. Links,
  hard-linked files and entries whose path changed while they were
  being opened get nothing. A link where a `.git` or denied entry should
  be refuses the run. The ACEs stay on the files after the run, and go
  with a file you move elsewhere on the same drive. To take them away,
  remove the container's SID (the node log's "GPU run" lines name the
  workspace; `Get-Acl` shows the SID, `S-1-15-2-…`) with
  `icacls <dir> /remove:g *<SID> /T /L`.
- It has no network unless `gpu_run.network` is `true`, which opens
  outbound internet connections only (`internetClient`): not your LAN,
  not inbound. Loopback is closed either way, so it can't reach the
  node's local API.
- It runs in a job with UI limits: no access to other programs' windows,
  no clipboard, no new or switched desktops, no logoff or shutdown, no
  system-wide settings. Low integrity also blocks sending input or
  messages to your programs (UIPI). Display mode changes stay allowed,
  for full-screen games.
- It gets a fixed environment (system paths and the container's folders)
  plus `env`, never the node's own. Its stdin is `NUL`. When the command
  exits or its time limit passes (default 5 minutes, at most
  `gpu_run.max_seconds`, up to 30), the job ends everything it started.

Refused: sessions with a `read-only` sandbox; sessions working in a
volume root, in your home directory or above it, inside a denied path,
or in or above Windows, `Program Files`, `ProgramData` or the
AppContainer folders; a `cwd` outside the working directory; batch files
(run them through `cmd.exe /d /c`); and programs outside the working
directory and the Windows and `Program Files` folders. Windows opens a
program's executable with the node's rights, not the container's, so
without that last rule a run could start any program in your profile.
Each session can have one run at a time.

Hard-linked files get no access: inside `.git` (after `git clone
--local`, say) they get a deny, which also applies under their other
names, so git inside the run may fail there. Package stores that hard-link
(pnpm, some cargo setups) are unreachable from runs. A program that
misbehaves only in a run (input methods, full-screen swap chains) may be
hitting the job's limit on other processes' windows; report it.

What stays possible, so weigh it before turning this on:

- Windows appear on your screen, and a program could draw something
  that looks like a password prompt. Don't type credentials into
  windows you didn't open.
- At low integrity a program can probably still read the screen (GDI
  capture), and so save what you have open into the workspace for the
  agent to read.
- It can read whatever Windows grants all AppContainers. That is system
  files, not your data, unless you have granted `ALL APPLICATION
  PACKAGES` access somewhere yourself.
- It talks to the GPU driver, a large kernel attack surface, as any GPU
  program does.

Direct3D in an AppContainer is what UWP games do, but test your program
there before relying on it. Also check on the host that a run cannot
change the ACL of a `.git` file you own (the owner's implicit rights
should not apply to the container, but verify it before trusting
`.git` protection). A program that writes its saves under your
profile should take its paths from `APPDATA` or `LOCALAPPDATA`; anything
that reads the real profile through known-folder APIs will get "access
denied".

## Git for agents: workspace_git

`workspace_git` is a fleet tool that the session's own node carries out,
outside the sandbox, as you (the node's user). It is not sent to the
coordinator. It works only on the session's own repository: the one its
working directory is in. Agents are told to `sync` before starting work
and whenever main moves, and to commit and push through it when shell git
fails, and always on Windows.

| Op | What it does |
| --- | --- |
| `status` | branch, upstream, ahead/behind, changed files |
| `sync` | fetch, then bring the current branch onto `remote`/`branch` (default: its upstream, else `origin`'s default branch). `strategy` `rebase` (default) rebases local commits, and on a conflict aborts and reports the files, changing nothing; `ff-only` refuses when diverged. Uncommitted changes, untracked files included (ignored ones stay where they are), are saved first and reapplied with a three-way merge; on a conflict the files get conflict markers and are listed |
| `commit` | `message`; `paths` (default: all changes, untracked included); `new_branch` to create or switch to a branch first. Hooks don't run |
| `push` | `branch` (default the current one) to its remote, with the upstream set. Never forced; never `main`, `master` or the remote's default branch (asked of the remote at each push) |
| `branch_delete` | the local branch; with `delete_remote`, also on the remote, only if `workspace_git` pushed it |

Every result carries the repository's status afterwards. Before a sync
touches the work tree it saves the uncommitted work as a stash-like commit
(in `git stash list`, "workspace_git sync of …") and as a binary patch in
`git/saved/` in the node directory (the newest 50 per repository), and
reports both. If anything fails halfway, the work is put back, and the
saved copies stay either way.

### Setting it up

Without configuration, `status`, `sync` and `commit` work: fetches use
whatever anonymous access the remote allows. `push` needs a token of its
own, never yours. Create a fine-grained GitHub token for the agents'
repositories only (Contents and Pull requests read/write), log `gh` in
with it into a directory of its own, and name that directory in
`node.json`:

```json
"git": {
  "gh_config_dir": "~/.config/swk/agent-gh",
  "push_branch_prefixes": ["swk/<host>/"]
}
```

| Key | What it does |
| --- | --- |
| `gh_config_dir` | A gh config directory whose `hosts.yml` holds the agent token. Required for `push` (and `branch_delete` with `delete_remote`). `~/` is the home directory. |
| `push_branch_prefixes` | Optional: the only branch name prefixes `workspace_git` pushes. `<host>` and `<session>` stand for the host and the session's name. Default: any branch but the default branch, `main` and `master`. |

Restart the node after changing them. On each OS:

- **Linux and macOS:**
  `GH_CONFIG_DIR=~/.config/swk/agent-gh gh auth login --with-token < token.txt`.
  Keep the directory out of the sandbox's reach (a `deny` in the Codex
  permission profile), since agents don't need to read it.
- **Windows:** in PowerShell,
  `$env:GH_CONFIG_DIR="$HOME\.config\swk\agent-gh"; Get-Content token.txt | gh auth login --with-token`.
  Git for Windows and `gh` must be on the node's `PATH`. git runs as the
  node, unsandboxed, so `sh.exe` works. The Windows sandbox can read
  what you can, so agents could read the token there; scope it to their
  repositories.

The node runs git with `-c credential.helper= -c "credential.helper=!gh auth
git-credential"` (gh as found on its `PATH`) and `GH_CONFIG_DIR` set to
that directory, and removes `GH_TOKEN`, `GITHUB_TOKEN` and the other
`GH_*` variables from git's environment. Without the setting it still
clears every credential helper, so neither fetch nor push ever reaches
your credential manager, keyring or `gh` login.

### What it refuses and why

The work tree is the agent's, and on some hosts `.git` may be writable,
so the node does not let either choose a command it runs:

- **Other repositories.** `path`, if given, must be inside the session's
  working directory and in the same repository; a nested repository is
  refused. `.git` must be a real directory at the top of the work tree,
  or the `.git` file of a normal linked worktree: its gitdir is
  `<repository>/worktrees/<name>`, with a `commondir` back to the
  repository and a `gitdir` back to this `.git` file. A `.git` file
  pointing anywhere else (a repository the agent made in `/tmp`, another
  worktree's gitdir) is refused. git then gets `GIT_DIR` and
  `GIT_WORK_TREE` and does no discovery of its own.
- **Commands from the repository.** The repository's config is trusted as
  yours, since agents cannot write `.git/config` (read-only on Windows
  and in the Linux profile). Still, every git runs with hooks pointed at
  an empty directory of the node's, and with `core.fsmonitor=false`,
  `core.sshCommand`, `core.askPass`, `core.alternateRefsCommand`,
  editors, signing and `http.extraHeader` overridden, submodules not
  recursed and auto-gc off. A repository whose own config includes another
  file (`include.path`, `includeIf`) or defines a filter, merge or diff
  driver is refused, because `.gitattributes` in the work tree picks
  which driver runs. Your global and system config are used as they are
  (Windows needs `http.sslBackend` from the system config).
- **Other transports.** Only https is spoken (`protocol.allow=never`,
  `protocol.https.allow=always`, `protocol.file.allow=never`). GitHub ssh
  remotes (`git@github.com:…`, `ssh://git@github.com/…`) are reached over
  https through `url.https://github.com/.insteadOf` on git's command line,
  so the repository's config is left alone; other ssh remotes are mapped
  to https on the same host. Your ssh keys are never used.
- **Pushes.** No force, no refspecs: the branch name is checked with
  `git check-ref-format`, and `main`, `master`, the remote's default
  branch and names outside `push_branch_prefixes` are refused. The
  upstream is recorded after the push by the node (which can write
  `.git/config`), so nothing in the sandbox has to.
- **Author.** Commits take `user.name` and `user.email` from the
  repository's or your config; an empty name, or one with `$(`, backquotes
  or angle brackets in it, is replaced by `<host>/<session>
  <noreply@switchworks.invalid>`.

Each op has a time limit (1 minute for `status`, 5 for `sync` and
`push`); git is run directly, with no shell, and never prompts
(`GIT_TERMINAL_PROMPT=0`).

## Images and the vision probe

Messages and first prompts can carry images ([messaging.md](messaging.md#images)
has the limits and the rules for files agents attach). Per harness:

- **Codex**: each image becomes a `localImage` input. The node writes it to
  a private file (0600) in the session's scratch directory
  (`<node dir>/scratch/<session>`, 0700) named after its digest, and
  app-server reads it and encodes it for the model, as the Codex TUI does
  for pasted images. The file is removed when the turn completes, when the
  node restarts and when the session stops. The schema also accepts an
  `image` input with a `data:` URL, but app-server passes that through
  unchanged and echoes it in the turn's `userMessage` item (Switchworks
  replaces any such URL in the event log); `localImage` keeps the bytes
  out of the protocol stream. Steering with images works too.
- **Goose and other ACP agents**: ACP image content blocks, only when the
  agent advertises `promptCapabilities.image` (see below).
- **agy**: refused. agy's headless docs say of the stream-json input that
  "text is the only supported block type"; any other block ends the
  session.

A harness that takes images does not mean the model sees them. A model
without vision, a provider without image input, or an "OpenAI- or
Anthropic-compatible" endpoint that ignores image parts leaves the model
with the text alone, and it may then make something up. Check each
host's configuration with the vision probe:

```sh
swk dev vision-probe                           # codex with its configured model
swk dev vision-probe --model <model>
swk dev vision-probe --harness goose           # goose with its configured provider
swk dev vision-probe --harness goose-deepseek --acp-bin goose --acp-args acp    # or any ACP agent
```

It draws a random six-character code into a PNG (with a built-in bitmap
font), sends it with "Reply with only the code in this image." in a new
session in an empty temporary directory, and prints PASS or FAIL with the
model's answer. It runs the harness directly with your own configuration
and keys, like `swk dev run`, so a goose configured in `node.json` needs
the same environment (or its own `goose configure`). `--save FILE` keeps
the image.

The probe image also carries its code in a PNG text chunk that no real
model reads. The mock provider (`swk dev mock-provider`) and the fake
harnesses in the tests read it there, so the probe and the whole image
path are tested in CI without a model:

```sh
swk dev mock-provider --codex-home /tmp/mock-home &
swk dev vision-probe --codex-home /tmp/mock-home      # PASS: the image reached the "model"
```

Both of the mock's emulations take images: `input_image` data URLs
(OpenAI Responses, from Codex) and base64 `image` blocks (Anthropic
Messages, from goose). Real goose 1.54.0 passes the probe through the
mock's Anthropic emulation.

With real models (2026-10-16 fleet test), DeepSeek V4.1 Flash and MiMo
V2.6 Pro both see images in Codex and in goose (MiMo in goose only with
`supports_vision`). Both sometimes misread one character of the probe's
code (V/U, M/N/W), so run a failing probe twice before concluding; the
probe says when an answer was a near miss rather than no reading at all.

## ACP agents (Goose and others)

`goose` is built in: when `goose` is on PATH the node offers it with no
configuration. Add other ACP agents by name:

```json
"acp": {
  "opencode": {"bin": "opencode", "args": ["acp"]},
  "goose": {"disabled": true}
}
```

An entry may set `bin`, `args`, `env`, `modes` and `disabled`. `codex` and
`agy` are reserved names.

How a session maps onto ACP:

- **Process.** One agent process per session, started in the session's
  working directory. A crash affects only that session; the adapter starts
  a new process and reopens the session (`session/load`, else
  `session/resume`), with backoff. An agent that stops reading its input
  for 30 seconds is handled the same way.
- **Turns.** Each message is a `session/prompt`. Streamed agent text,
  thoughts, tool calls and plans become transcript items. Tool calls are
  shown the way Codex's are: shell commands as commands (with output and
  exit code), edits as file changes (with a unified diff, also collected
  into the turn's `turn.diff`), MCP tools as tool calls, anything else as a
  generic tool item.
- **Approvals.** `session/request_permission` becomes a Switchworks
  approval. Option kinds map to the familiar decisions: `allow_once` →
  `accept`, `allow_always` → `acceptForSession`, `reject_once` →
  `decline`, `reject_always` → `declineAlways`; `cancel` declines and
  cancels the turn. Calls to Switchworks's own fleet tools are approved
  automatically, as they are for Codex.
- **Approval policy.** The spawn's approval policy picks the agent's
  session mode: `never` → `auto`, `on-request` → `smart_approve`,
  `untrusted` → `approve` (goose's names; `bypassPermissions` and
  `default` are tried for other agents). Override with `"modes":
  {"never": "...", ...}`. When the agent offers session modes but none
  fits a policy stricter than `never` (`on-request`, `untrusted`), or
  setting it fails, the session is refused rather than left in the
  agent's default mode, which may run tools without asking. An agent with
  no session modes at all runs a person's session with its own
  configuration and a warning, and refuses one an agent spawned. Sandbox
  settings do not apply to ACP agents.
  goose's `smart_approve` asks about more than Codex's `on-request` does,
  reads such as `cat` included: expect several approvals per task. goose
  can also ask about two tool calls at once.
- **Model and provider.** `--model` and `--provider` are applied through
  the agent's `model` and `provider` session config options when it offers
  them; a value the agent rejects is a warning, and the session continues
  with the agent's default.
- **Interrupt.** `session/cancel`; pending approvals are answered
  "cancelled". An agent that ignores the cancel for 20 seconds is killed
  and the session reopened.
- **Stop.** Cancels any turn, sends `session/close` if the agent supports
  it, and stops the process. The agent keeps its history. Stopping the
  node does not close sessions, so they reopen when it starts again.
- **Usage.** From goose's own accumulated session usage when it sends it:
  Switchworks opts in to goose's custom notifications in `initialize`
  (`_meta.goose.customNotifications`), and goose 1.55 then reports the
  session's summed input and output tokens over every model request in
  `_goose/unstable/session/update`. Otherwise from the prompt response's
  `usage` (an unstable ACP field), which goose fills with its last model
  request only, so a turn with several tool calls is undercounted, and a
  turn that fails or is interrupted (no prompt response) counts only
  through goose's accumulated usage. The context window comes from
  `usage_update`. Token counts survive node
  restarts: the coordinator adds each report's growth to the session.
- **Fleet tools.** Attached per session through `session/new`'s
  `mcpServers`, with the session's credentials in the server's
  environment. Nothing is written to disk.

- **Images.** Sent as ACP `image` content blocks (`{"type": "image",
  "data": <base64>, "mimeType": ...}`) after the prompt's text, only when
  the agent's `initialize` response advertises
  `promptCapabilities.image` (goose 1.54.0 does:
  `{"image": true, "audio": false, "embeddedContext": true}`). For an
  agent that does not, a message with images is refused before delivery,
  and a spawn whose prompt has images fails.

Not supported yet: audio and embedded resources in prompts, ACP terminals
and client-side file access (agents use their own tools), authentication
flows (configure the agent's provider beforehand), and observing sessions
started outside Switchworks.

## agy (Antigravity CLI)

Built from agy's documented headless mode, tested against a fake that
emits the documented events, and run against agy 1.4.0 in the 2026-10-16
fleet test. Record a stream with `swk dev run --harness agy --record
agy.jsonl "hello"` and report anything that renders oddly.

`node.json`:

```json
"agy": {
  "bin": "agy",
  "args": [],
  "per_turn": false,
  "fleet_tools": false
}
```

- **Process.** One long-lived `agy --output-format stream-json
  --input-format stream-json` process per session; each message is a
  `{"event":"user",...}` line and each turn ends with its own `result`.
  `per_turn: true` runs one process per turn instead. Either way the
  conversation continues with `--conversation <id>`.
- **Transcript.** `step_update` events become items: agent text streams
  as a message, and tool steps become commands, file changes, MCP tool
  calls or generic tool items by tool name.
- **Permissions.** Headless agy never asks: a tool that needs confirmation
  is denied, and Switchworks shows agy's notice as a warning. The spawn's
  approval policy picks agy's mode:

  | Sandbox | Approval policy | agy flags | What runs without an allow rule |
  | --- | --- | --- | --- |
  | `danger-full-access` | any | `--dangerously-skip-permissions` | everything |
  | `workspace-write` (or none) | `never`, `on-request`, `on-failure` | `--mode accept-edits` | reading and editing files |
  | `workspace-write` (or none) | `untrusted` (or none) | agy's default, request-review | reading files |
  | `read-only` | any | agy's default, request-review | reading files |

  `read-only` and `workspace-write` also add `--sandbox`, but that only
  restricts agy's terminal: its edit tools write wherever the user can,
  inside the workspace or not. So permissions are skipped only with
  `danger-full-access` (approval policy `never` alone no longer does it),
  and a read-only session never gets `accept-edits`. A `--mode` in `args`
  overrides the mapping (and with it this protection). agy's `--mode auto`
  does not help headless sessions: commands and MCP calls were still
  denied. To let a session run commands without skipping permissions,
  allow them in `~/.gemini/antigravity-cli/settings.json`, the only
  settings file agy reads (`"permissions": {"allow": ["command(git)", "command(python3)"]}`).
  Do not allow `mcp(swk/*)` there: it would pre-approve the tools of any
  MCP server named `swk`, including one an untrusted repository defines in
  its `.agents/mcp_config.json` (the fleet tools use names of their own,
  below).
- **Spawned by an agent.** Because of the above, the coordinator treats agy
  as not enforcing a sandbox: an agent of another harness may spawn agy
  only if it has `danger-full-access` itself, or `allow_unsandboxed_spawn`
  is set (and, since headless agy never asks, only with approval policy
  `never`, or `allow_unapproved_spawn`). See [messaging](messaging.md).
- **What to expect.** With permissions skipped, agy 1.4.0 finished the
  fleet test's coding task correctly, but took about eleven minutes and
  ten times the tokens Codex and goose used, and read other conversations'
  transcripts under `~/.gemini/antigravity-cli/brain/` along the way.
  Give it full access only in workspaces and on machines where that is
  acceptable.
- **Interrupt** stops the process; the next message starts a new one on
  the same conversation. The turn is reported as interrupted.
- **Usage and duration.** agy reports both cumulatively per process in
  stream mode; Switchworks records the per-turn difference and times turns
  itself. agy's input count excludes cache reads, which are added in so
  agy's figures compare with Codex's. Usage comes only with a turn's
  `result`: an interrupted turn still gets one (agy ends the turn when its
  stdin closes), but a turn whose process crashes or is killed before
  writing it is not counted.
- **Fleet tools** (`fleet_tools: true`): agy has no per-run MCP option, so
  for a new session the node adds an entry named `swk-` and 16 random hex
  digits (exactly `command` and `args`, nothing else) to
  `<cwd>/.agents/mcp_config.json`, keeping any other entries, and lists
  the file in `.git/info/exclude` if it created it. Sessions running in
  the same workspace share that entry. Unless the session skips
  permissions, the node also adds `mcp(swk-<those digits>/*)` to
  `permissions.allow` in agy's global settings, and says so in a warning:
  only that name is pre-approved, so a server another workspace calls
  `swk` (or anything else) is not, and no workspace can define this one
  ahead of time. The rule is removed when the last session in the
  workspace ends, whether it is stopped or the node shuts down; rules
  left by a node that crashed are removed the next time the node sets up
  fleet tools, and a leftover `mcp(swk/*)` from earlier versions is
  reported in a warning (remove it yourself). When the last session in the
  workspace is stopped, the node also removes the entry it added (and the
  file and exclude line, if it created them). A node shutdown leaves the
  entry, and the session resumed there takes it over (adding the rule
  again) if it is still exactly the node's command and args; otherwise,
  sessions resumed by the node are not given fleet tools in their
  workspace. The session's credentials reach `swk mcp` through agy's
  environment, so every command and MCP server agy runs in that session
  can see the session's fleet token. The node refuses to write through a
  symlinked `.agents`, and only appends to `info/exclude` of a real git
  directory. The transcript shows the tools under `swk` as usual.
