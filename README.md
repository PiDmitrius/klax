# klax

`klax` is a messenger bridge for coding agents. It connects Telegram, MAX, VK, and Yandex Messenger chats to a local CLI backend and streams progress back into the chat.

Supported backends:

- `claude` via Claude Code CLI
- `codex` via OpenAI Codex CLI

The daemon keeps per-chat sessions, resumes them across restarts, runs the agent in the session working directory, and sends intermediate tool activity while the answer is being built.

## How It Works

```text
Messenger -> klax daemon -> agent CLI -> Messenger
```

At a high level:

1. `klax` polls enabled messengers.
2. It maps the incoming chat to a stored session.
3. It starts or resumes the selected backend in that session's working directory.
4. It streams tool activity and the final result back to the messenger.

## Features

- Telegram, MAX, VK, and Yandex Messenger transports
- `claude` and `codex` backends
- Persistent sessions with resume support
- Per-session backend, model, thinking level, and sandbox mode
- Group mode with a dedicated working directory per group chat
- systemd user service management
- Release update flow, plus local-source rebuilds via `source_dir`

## Requirements

- Linux (`amd64` or `arm64`)
- `systemd --user`
- At least one configured backend:

### Claude backend

Install and authenticate Claude Code CLI:

```bash
curl -fsSL https://claude.ai/install.sh | bash
```

### Codex backend

Install Codex CLI:

```bash
npm install -g @openai/codex
```

Codex must be authenticated before use (e.g. via `OPENAI_API_KEY` or `codex auth`).

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/PiDmitrius/klax/main/install.sh | bash
```

The installer places the binary in `~/.local/bin/klax`, checks PATH wiring, and prepares the user environment for `systemd --user`.

## Quick Start

```bash
klax setup
klax install
klax start
```

`klax setup` creates or updates `~/.config/klax/config.json` interactively. Press `Enter` to keep the current value, or enter `-` to clear it.

## Configuration

Main config file:

- `~/.config/klax/config.json`

Minimal example:

```json
{
  "tg_token": "123456:AAH...",
  "tg_allowed_users": [123456789],
  "default_cwd": "/home/user/work",
  "default_backend": "claude",
  "backends": {
    "claude": {},
    "codex": {}
  },
  "source_dir": ""
}
```

Common fields:

| field | description |
|---|---|
| `tg_token` | Telegram bot token |
| `tg_allowed_users` | Telegram whitelist |
| `mx_token` | MAX bot token |
| `mx_allowed_users` | MAX whitelist |
| `vk_token` | VK group token |
| `vk_allowed_users` | VK whitelist |
| `ym_token` | Yandex Messenger bot token |
| `ym_allowed_users` | Yandex Messenger whitelist (logins, e.g. `vasya@example.org`) |
| `default_cwd` | working directory for new direct-message sessions |
| `default_backend` | default backend for new sessions: `claude` or `codex` |
| `source_dir` | local klax source tree used by `klax update` for local builds |
| `users` | optional cross-platform identity mapping for shared DM sessions |
| `audit` | optional synchronous per-turn JSON audit hook |

Runtime backend settings such as backend selection, model, thinking level, and sandbox mode are configured per session from chat via `/settings`.

### Turn audit hook

Configure a local executable as an argument array:

```json
{
  "audit": {
    "turn": {
      "start": {
        "command": ["/usr/local/bin/audit-turn-start"]
      },
      "finish": {
        "command": ["/usr/local/bin/audit-turn-finish"]
      }
    }
  }
}
```

The executable is started directly, without a shell. It receives one JSON
document on stdin and must exit before processing continues. Klax invokes it
twice per executed turn:

- `turn.start` after the final prompt and launch options are known, immediately
  before the backend starts;
- `turn.finish` after the result and session state are complete, immediately
  before final delivery.

The stable protocol is defined by
[`docs/audit-v1.md`](docs/audit-v1.md) and its machine-readable companion
[`docs/audit-v1.schema.json`](docs/audit-v1.schema.json).
The same deterministic 160-bit `turn.turn_id` correlates both calls.
Klax does not impose a timeout and does not cancel a running hook: a hook that
needs either behavior must implement it itself or configure a wrapper in
`command`. A stuck hook therefore blocks that turn by design.

The start hook is an execution gate: if it fails, klax records a durable
`audit-start-failed` system error and does not invoke the backend. The finish
hook reports an already completed computation: if it fails, klax records and
shows a durable system warning, logs the diagnostic, and still performs final
delivery.

The `turn.finish` boundary precedes the final rendered delivery, but it is not a
release gate: transport progress edits and the web transcript can already have
shown partial or complete content.

Audit documents are highly sensitive. They can contain raw messages, effective
prompts, answers, sender identities, working directories, system prompts, and
attachment metadata. The configured executable also inherits the klax process
environment. On failure, its bounded stderr is included in the klax log, so a
hook must never echo its input there. Treat the command and its destination as
part of the trusted klax deployment.

Klax constructs and hashes the audit snapshot only when at least one audit
command is explicitly configured. Event consumers, including storage and
forwarding integrations, live outside this repository.

A process crash after `turn.start` can leave that event without a matching
`turn.finish`; klax does not replay a backend turn after its durable run fence.
Consumers should treat an old unmatched `turn.start` as an interrupted turn,
not as proof that it is still running.

## Chat Commands

Primary commands available in messenger chats:

| command | effect |
|---|---|
| `/status` | show active session, runner status, queue length |
| `/sessions` | list sessions for the current chat/user |
| `/new [name]` | create a new session |
| `/settings` | choose backend, model, thinking level, and sandbox mode |
| `/name <name>` | rename the active session |
| `/cleanup` | session cleanup UI |
| `/cwd [path]` | show or change the active session working directory |
| `/prompt [text]` | show or set append system prompt |
| `/groups` | list or manage group mode |
| `/transports` | list or enable/disable transports |
| `/abort` | stop the current run and clear the queue |
| `/update` | trigger daemon update |
| `/help` | show built-in help |

Anything that is not recognized as a control command is forwarded to the active backend.

## CLI Commands

```text
klax setup       interactive first-time setup
klax install     install systemd user service
klax uninstall   remove systemd user service
klax start       start the service (--foreground to run directly)
klax stop        stop the service
klax restart     restart the service
klax update      update from GitHub release or rebuild from source_dir
klax fallback    install latest GitHub release, ignoring source_dir
klax status      show service status
klax version     print version
```

## Storage

Session state is stored in:

- `~/.local/share/klax/sessions.json`

Config is stored in:

- `~/.config/klax/config.json`

Each session stores:

- backend session ID (`claude` session UUID or `codex` thread ID)
- session name
- working directory
- selected backend
- model, thinking, and sandbox overrides
- counters and context metadata

Direct-message sessions are keyed by user identity. With `users` mapping configured, one person can share the same DM sessions across transports.

## Update Flow

`klax update` behaves in one of two ways:

- If `source_dir` is empty, it downloads the latest GitHub release and installs it.
- If `source_dir` is set, it rebuilds from local source and installs that binary instead.

The daemon watches for the restart marker, finishes the current task, notifies chats, exits, and relies on `systemd --user` to come back up.

## systemd Service

`klax install` writes a user service based on [klax.service](./klax.service):

- `ExecStart=%h/.local/bin/klax start --foreground`
- `Restart=always`
- `RestartSec=5`
- `StartLimitBurst=3`
- `StartLimitIntervalSec=60`

### Troubleshooting

If klax crashes 3 times within 60 seconds, systemd stops restarting it. To investigate and recover:

```bash
klax status                            # see the error
journalctl --user -u klax --no-pager   # full logs
systemctl --user reset-failed klax     # clear the failure counter
klax start                             # try again
```

Common causes: invalid bot token, network unreachable at startup, broken config. Check `~/.config/klax/config.json` and re-run `klax setup` if needed.

## Project Structure

```text
cmd/klax/           daemon, CLI entrypoints, chat command handling
internal/config/    config load/save and normalization
internal/session/   session store and scope defaults
internal/runner/    backend adapters, streaming parser, tool formatting
internal/tg/        Telegram transport
internal/max/       MAX transport
internal/vk/        VK transport
internal/ym/        Yandex Messenger transport
```
