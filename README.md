# klax

`klax` is a messenger bridge for coding agents. It connects Telegram, MAX, and VK chats to a local CLI backend and streams progress back into the chat.

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

- Telegram, MAX, and VK transports
- `claude` and `codex` backends
- Persistent sessions with resume support
- Per-session backend, model, thinking level, and sandbox mode
- Group mode with a dedicated working directory per group chat
- User service management: `systemd --user` on Linux, `launchd` LaunchAgent on macOS
- Release update flow, plus local-source rebuilds via `source_dir`

## Requirements

- Linux (`amd64` or `arm64`) with `systemd --user`, **or** macOS (`arm64` or `amd64`) with `launchd`
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

The installer detects the OS, places the binary in `~/.local/bin/klax`, checks PATH wiring, and prepares the user environment (`systemd --user` on Linux, `launchd` on macOS).

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
| `default_cwd` | working directory for new direct-message sessions |
| `default_backend` | default backend for new sessions: `claude` or `codex` |
| `source_dir` | local klax source tree used by `klax update` for local builds |
| `users` | optional cross-platform identity mapping for shared DM sessions |

Runtime backend settings such as backend selection, model, thinking level, and sandbox mode are configured per session from chat via `/settings`.

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
| `/bypass ...` | send a direct backend command |
| `/abort` | stop the current run and clear the queue |
| `/update` | trigger daemon update |
| `/help` | show built-in help |

Anything that is not recognized as a control command is forwarded to the active backend.

## CLI Commands

```text
klax setup       interactive first-time setup
klax install     install the user service (systemd on Linux, launchd on macOS)
klax uninstall   remove the user service
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

The daemon watches for the restart marker, finishes the current task, notifies chats, exits, and relies on the supervisor (`systemd --user` on Linux, `launchd` `KeepAlive` on macOS) to come back up.

## Service Management

`klax install`, `start`, `stop`, `restart`, `status`, and `uninstall` wrap the platform service manager. The same CLI works on both platforms; only the supervisor underneath differs.

### Linux (systemd)

`klax install` writes a user service based on [klax.service](./klax.service):

- `ExecStart=%h/.local/bin/klax start --foreground`
- `Restart=always`
- `RestartSec=5`
- `StartLimitBurst=3`
- `StartLimitIntervalSec=60`

If klax crashes 3 times within 60 seconds, systemd stops restarting it. To investigate and recover:

```bash
klax status                            # see the error
journalctl --user -u klax --no-pager   # full logs
systemctl --user reset-failed klax     # clear the failure counter
klax start                             # try again
```

### macOS (launchd)

`klax install` writes a LaunchAgent to `~/Library/LaunchAgents/klax.plist`:

- `ProgramArguments`: `~/.local/bin/klax start --foreground`
- `KeepAlive` (mirrors `Restart=always`) and `ThrottleInterval=5` (mirrors `RestartSec=5`)
- `RunAtLoad` so it starts on login
- `EnvironmentVariables.PATH` seeded with Homebrew prefixes and `~/.local/bin` so the backend CLIs resolve under launchd's minimal environment

Logs go to `~/Library/Logs/klax.log`. To investigate:

```bash
klax status                            # launchctl print for the service
tail -f ~/Library/Logs/klax.log        # full logs
klax restart                           # kill and relaunch
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
```
