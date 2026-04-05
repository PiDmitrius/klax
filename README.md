# klax

Telegram bridge for Claude Code. Uses your Claude Max subscription — no extra API billing.

## How it works

```
Telegram → klax daemon → claude -p --output-format stream-json → Telegram
```

klax polls Telegram, routes messages to Claude Code CLI, streams tool activity back as the response is built, and delivers the final answer.

## Install

```bash
go install github.com/PiDmitrius/klax/cmd/klax@latest
klax setup
klax install
klax start
```

## Setup

`klax setup` writes `~/.config/klax/config.json`:

```json
{
  "telegram_token": "123456:AAH...",
  "allowed_users": [123456789],
  "default_cwd": "/home/user/projects",
  "permission_mode": "bypassPermissions",
  "source_dir": "/home/user/workspace/klax"
}
```

| field | description |
|---|---|
| `telegram_token` | Bot API token from @BotFather |
| `allowed_users` | Telegram user IDs (whitelist) |
| `default_cwd` | working directory for new sessions |
| `permission_mode` | `acceptEdits` (default), `bypassPermissions`, or `auto` |
| `source_dir` | path to klax source for local builds (`klax update`) |

## Update

```bash
klax update
```

Builds from `source_dir`, installs the new binary, and writes a restart marker. The running daemon picks up the marker, drains the current task, notifies all chats, and exits. systemd restarts it with the new binary.

## Telegram commands

| command | effect |
|---|---|
| `/status` | active session, current tool, queue length |
| `/sessions` | list all sessions |
| `/new [name]` | create new session |
| `/switch <n>` | switch to session by number |
| `/clear` | reset context of active session |
| `/abort` | kill current Claude process and clear queue |
| `/help` | command reference |

Everything else is forwarded to Claude.

## CLI commands

```
klax setup       interactive first-time setup
klax install     install systemd user service
klax uninstall   remove systemd user service
klax start       start the service (--foreground to run directly)
klax stop        stop the service
klax restart     restart the service
klax update      build, install, and restart (from source)
klax status      show service status
klax version     print version
```

## Sessions

Sessions are stored in `~/.local/share/klax/sessions.json`. Each session has:
- a Claude session UUID (persisted across restarts via `--resume`)
- a working directory
- a name

Claude runs in the session's working directory with `--resume <uuid>`.

## systemd

`klax install` sets up a user service at `~/.config/systemd/user/klax.service`:
- `Restart=always` — auto-restart on exit
- `StartLimitBurst=3` / `StartLimitIntervalSec=60` — stops retrying after 3 crashes in 60s
- Validates Telegram token on startup (fatal on bad token, retries on network errors)
- Drains stale messages on startup to skip accumulated updates

## Structure

```
cmd/klax/           daemon, CLI commands
internal/config/    config.json read/write
internal/runner/    claude process, stream-json parser, tool formatter
internal/session/   session store (sessions.json)
internal/tg/        Telegram Bot API client (no external deps)
```
