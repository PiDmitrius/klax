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

## Session control API

The UI HTTP server also serves programmatic clients. A browser is not required.
Requests use the existing `Authorization: Bearer <api-token>` header and the
same user scope, sessions, execution queue, settings and results as the UI.

### Create a session

`POST /api/new` accepts the initial session settings: `name`, `cwd`, `backend`,
`model`, `think`, `sandbox`, `tty`, `prompt` and `groups`. Settings are validated
before the session and its defaults are saved and published. A failed request
creates no session. An empty body creates a session with the scope defaults.

```json
{
  "name": "developer-01",
  "cwd": "/work"
}
```

The response is `200` with `{"created":42}`. `created` is the persistent klax
session identifier used by all subsequent requests. Backend session identifiers
are managed internally.

Each backend process receives `KLAX_SESSION_ID` in its environment, containing
this persistent identifier. Launch wrappers and scripts invoked by the backend
can use it as the `session` field in `/api/send`; the backend must preserve the
variable when launching tools. The API URL and the owning user's management
token are still required. A running turn sending a new message to its own session
must use `return_on: "queued"`: waiting for that message to start or finish would
block on the current turn's completion.

### Access roles and read markers

Each configured user may have two independent credentials:

```json
{
  "id": "operator",
  "ui_token": "<management-token>",
  "ui_read_token": "<viewing-token>"
}
```

Both tokens authenticate through `Authorization: Bearer <token>` and address the
same user's sessions. Tokens must be unique across all users and roles.
`ui_token` grants ordinary management access. `ui_read_token` permits viewing
sessions, events, settings and files, plus updating its own read markers.
Creation, messages, attachments, abort, deletion, settings changes, tab reordering
and service updates are rejected by the server with `403 read-only`.
Read access does not create an initial session in an empty account.

`GET /api/auth` returns `user` and `read_only`. Session and settings views also
include `read_only`. The viewing UI hides the new-session button, keeps its
composer gray and disabled, and disables mutation controls. Messenger permissions
and backend filesystem permissions are independent of the UI access role.

Management and viewing access have separate persistent read-through watermarks
for every session. Browsers sharing a role synchronize their markers; reading
through one role does not mark the other role's messages as read. Markers survive
a restart and token rotation. The watermark uses the same `(turn, block)` axis
and unread-count calculation for both roles.

For automatic login, open `https://<host>/<mount>/#login=<viewing-token>` with a
URL-encoded token. The browser consumes and removes the fragment before making
API requests, and uses the supplied token in preference to any saved credential.
Anyone holding the link can read that user's sessions. An invalid saved token
returns the UI to the login form; clearing application data is unnecessary.

`control_token` on `/api/new` is unsupported and returns
`400 unsupported-control-token`. There is no per-session control-token check;
`X-Klax-Control-Token` grants no permissions.

### Send a message

`POST /api/send` accepts JSON or multipart form data. Multipart retains the
`files` attachment field; scalar fields have the same names as in JSON.

```json
{
  "session": 42,
  "text": "Perform the task",
  "nonce": "client-message-1",
  "return_on": "finish"
}
```

| `return_on` | Response boundary |
| --- | --- |
| `queued` (default) | Durable queue acceptance; `204 No Content`. |
| `start` | Preparation, durable run registration and the optional start hook complete; `200` with `turn.start`. |
| `finish` | Backend execution, result formation and the optional finish hook complete; `200` with `turn.finish`. |

Both event responses use [klax.audit/v1](docs/audit-v1.md). The API and configured
hook receive the same event snapshot. Waiting also works with no hooks.
`start` permits execution; it does not guarantee a successful backend launch.
A completed backend error or cancellation returns a finish event with
`turn.result.status` equal to `error` or `aborted`. The existing normalized
trace and raw-file range reference are retained; raw history is not embedded.

`nonce` is optional. Omission generates a unique nonce. An explicit value must
be a nonempty string. Repeating a nonce in the same session never starts another
execution: `queued` confirms the existing acceptance; `start` and `finish`
join or return that turn's result retained in this process. Each session retains
up to 64 completed turn results; pending turns and already attached waiters are
not evicted. If the requested
boundary is unavailable, the response is `409` with `result-unavailable`.
Requests without a supplied nonce are distinct messages and must not be
retried automatically.

### Waiting and failures

For `start` and `finish`, klax sends no headers or body until the selected
boundary or an error. The response contains exactly one JSON object; no
heartbeat or intermediate backend events are sent. Responses are not cached.
Klax sets no waiting timeout. Clients and any proxies must allow long periods
without response data, including waiting for the response headers. A connection
is not guaranteed to survive indefinitely.

Disconnecting stops only that client's wait. Accepted work continues, and a
slow or failed HTTP writer cannot block the executor or its queue. A connection
ending without a complete JSON response leaves the outcome unknown to the
client. Waiting is not restored across service restarts. Messages accepted
while draining are durable for replay; their waiting response is
`result-unavailable` because execution belongs to the next process.

Errors use their HTTP status, since headers are held until the response is
ready. Execution/preparation failures returned by the waiting API have this
shape:

```json
{
  "error": {
    "code": "audit-start-failed",
    "message": "Стартовый гейт отклонил выполнение"
  }
}
```

| Code | HTTP status | Meaning |
| --- | --- | --- |
| `read-only` | 403 | The authenticated token permits viewing only. |
| `session-not-found`, `session-deleted` | 404 | Session unavailable in the authenticated scope. |
| `invalid-nonce`, `invalid-return-on`, `unsupported-control-token`, `empty-message` | 400 | Invalid input; nothing enqueued or created. |
| `result-unavailable` | 409 | Boundary cannot be recovered in this process. |
| `aborted` | 409 | Waiting message removed from the queue. |
| `enqueue-failed` | 500 | Durable acceptance failed. |
| `attachments-missing`, `run-start-failed`, `audit-start-failed` | 500 | Preparation, registration or start gate failed. |
| `result-save-failed` | 500 | Result persistence failed. |

Existing authentication, parsing and settings errors retain their ordinary
HTTP error responses. All paths that stop a turn before the requested boundary
release its waiters with an error. Start-gate failure does not await or produce
a finish event. No new hook timeout is imposed: a blocked configured hook
continues to block its corresponding boundary.

A failed finish hook preserves the backend result and the existing UI warning.
The API returns the finish event with an additional top-level field:

```json
{
  "warnings": [
    {
      "code": "audit-finish-failed",
      "message": "Не удалось записать событие завершения хода в аудит"
    }
  ]
}
```

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

## Contract

The invariants that span more than one file — what owns a turn's state, where a failure's reason
comes from, how the live channel and the unread axis are defined — are listed in
[`docs/CONTRACT.md`](docs/CONTRACT.md). An invariant that governs a single file lives at the top of
that file instead.

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
