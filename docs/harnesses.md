# Harnesses: Codex, Goose and other ACP agents, agy

Switchyard manages harnesses; it is not one. Each node runs whichever of
these it finds, and `swyd hosts` (and the spawn form, and `fleet_list`)
shows what each host offers. Spawn with `--harness <name>`.

| Harness | Protocol | Approvals relayed | Steer | Fleet tools | Resume after node restart | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `codex` | `codex app-server` (JSON-RPC, stdio) | yes | yes | per thread (MCP) | `thread/resume` | done |
| `goose` | ACP over stdio (`goose acp`) | yes | no (queued) | per session (ACP `mcpServers`) | `session/load` | done; tested against goose 1.54.0 |
| other ACP agents | ACP over stdio | yes | no (queued) | per session | `session/load` or `session/resume` | experimental; tested with a fake ACP agent |
| `agy` | headless `stream-json` | no (agy never asks) | no (queued) | opt-in, via workspace config | `--conversation` | experimental; tested with a fake agy only |

Steering is a Codex feature. For the others, a message sent with `--steer`
is queued for the next turn.

## Models without a translation proxy

Codex only speaks the OpenAI Responses API. Of the providers below, only
MiMo offers it, so Codex needs a Responses-to-Chat proxy for DeepSeek and
GLM. Goose's `anthropic` provider speaks Anthropic Messages, which all three
offer natively:

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
            "ANTHROPIC_API_KEY": "…", "GOOSE_MODEL": "deepseek-v4-pro"}
  },
  "goose-mimo": {
    "bin": "goose", "args": ["acp"],
    "env": {"GOOSE_PROVIDER": "anthropic", "ANTHROPIC_HOST": "https://api.xiaomimimo.com/anthropic",
            "ANTHROPIC_API_KEY": "…", "GOOSE_MODEL": "mimo-v2.6"}
  }
}
```

`swyd spawn antec --harness goose-deepseek …` then runs that one. These
three endpoints have been exercised only through the mock provider's
Anthropic Messages emulation (`swyd dev mock-provider`), not with real keys;
check the model ids against each provider's current list. Keys can also
live in goose's own config (`goose configure`) instead of `node.json`,
which is plain text (0600).

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
- **Approvals.** `session/request_permission` becomes a Switchyard
  approval. Option kinds map to the familiar decisions: `allow_once` →
  `accept`, `allow_always` → `acceptForSession`, `reject_once` →
  `decline`, `reject_always` → `declineAlways`; `cancel` declines and
  cancels the turn. Calls to Switchyard's own fleet tools are approved
  automatically, as they are for Codex.
- **Approval policy.** The spawn's approval policy picks the agent's
  session mode: `never` → `auto`, `on-request` → `smart_approve`,
  `untrusted` → `approve` (goose's names; `bypassPermissions` and
  `default` are tried for other agents). Override with `"modes":
  {"never": "...", ...}`. Sandbox settings do not apply to ACP agents.
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
- **Usage.** From the prompt response's `usage` (an unstable ACP field;
  goose sends it) and the context window from `usage_update`.
- **Fleet tools.** Attached per session through `session/new`'s
  `mcpServers`, with the session's credentials in the server's
  environment. Nothing is written to disk.

Not supported yet: images and other non-text prompt content, ACP terminals
and client-side file access (agents use their own tools), authentication
flows (configure the agent's provider beforehand), and observing sessions
started outside Switchyard.

## agy (Antigravity CLI)

Built from agy's documented headless mode and tested against a fake that
emits exactly the documented events. It has not been run against a real
`agy` yet; record one with `swyd dev run --harness agy --record agy.jsonl
"hello"` and report anything that renders oddly.

`node.json`:

```json
"agy": {
  "bin": "agy",
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
- **Permissions.** Headless agy never asks. `--sandbox
  danger-full-access` passes `--dangerously-skip-permissions`; `read-only`
  and `workspace-write` pass `--sandbox`. Otherwise tools that need
  confirmation are soft-denied, and Switchyard shows agy's notice as a
  warning. Pre-allow tools in `~/.gemini/antigravity-cli/settings.json`
  (`"permissions": {"allow": ["command(git)", ...]}`).
- **Interrupt** stops the process; the next message starts a new one on
  the same conversation.
- **Usage** is cumulative per process in stream mode; Switchyard records
  the per-turn difference.
- **Fleet tools** (`fleet_tools: true`): agy has no per-run MCP option, so
  the node adds a `swyd` entry (command and args only) to
  `<cwd>/.agents/mcp_config.json`, keeping any other entries; lists the
  file in `.git/info/exclude` if it created it; and, unless the session
  skips permissions, adds `mcp(swyd/*)` to `permissions.allow` in agy's
  settings. The session's credentials reach `swyd mcp` through agy's
  environment, which assumes agy passes its environment to MCP servers it
  starts, as goose and Codex do. That is unverified.
