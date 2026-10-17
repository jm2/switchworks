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

Steering is a Codex feature. For the others, a message sent with `--steer`
is queued for the next turn.

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
  `session/resume`), with backoff.
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
  {"never": "...", ...}`. Sandbox settings do not apply to ACP agents.
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
  request only, so a turn with several tool calls is undercounted. The
  context window comes from `usage_update`. Token counts survive node
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

  | Approval policy | agy flags | What runs without an allow rule |
  | --- | --- | --- |
  | `untrusted` (or none) | agy's default, request-review | reading files |
  | `on-request`, `on-failure` | `--mode accept-edits` | reading and editing files |
  | `never` | `--dangerously-skip-permissions` | everything |

  Sandbox `read-only` and `workspace-write` add `--sandbox` (agy's terminal
  restrictions); `danger-full-access` skips permissions like `never`. A
  `--mode` in `args` overrides the mapping. agy's `--mode auto` does not
  help headless sessions: commands and MCP calls were still denied. To let
  a session run commands without skipping permissions, allow them in
  `~/.gemini/antigravity-cli/settings.json`, the only settings file agy
  reads (`"permissions": {"allow": ["command(git)", "command(python3)", "mcp(swk/*)"]}`).
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
  agy's figures compare with Codex's.
- **Fleet tools** (`fleet_tools: true`): agy has no per-run MCP option, so
  for each new session the node adds a `swk` entry (command and args only)
  to `<cwd>/.agents/mcp_config.json`, keeping any other entries, and lists
  the file in `.git/info/exclude` if it created it. Stopping the session
  removes both again. Unless the session skips permissions, the node also
  adds `mcp(swk/*)` to `permissions.allow` in agy's global settings, and
  says so in a warning; that rule stays. Sessions that already existed
  when you turned fleet tools on are not changed when the node resumes
  them. The session's credentials reach `swk mcp` through agy's
  environment, so every command and MCP server agy runs in that session
  can see the session's fleet token. The node refuses to write through a
  symlinked `.agents`, and only appends to `info/exclude` of a real git
  directory.
