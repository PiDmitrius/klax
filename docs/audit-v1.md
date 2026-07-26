# klax audit protocol v1

This document is the normative contract for local klax audit hooks. The JSON
Schema in `audit-v1.schema.json` is its machine-readable companion. Where the
two disagree, the discrepancy is a bug.

## Purpose

Audit hooks synchronously report security-relevant turn boundaries. The start
hook is an execution gate: klax may invoke the backend only after that hook
exits successfully. The finish hook reports an already completed computation;
its failure is made visible but cannot undo the turn.

The v1 protocol defines two events:

- `turn.start`: klax has durably accepted and prepared the turn and is about to
  invoke the backend.
- `turn.finish`: klax has completed and persisted the backend turn and is about
  to perform its final delivery step.

Neither event asserts that the final result was delivered to a transport.

## Hook invocation

Each event has an independently configured command:

```json
{
  "audit": {
    "turn": {
      "start": {
        "command": ["/path/to/auditor", "turn.start"]
      },
      "finish": {
        "command": ["/path/to/auditor", "turn.finish"]
      }
    }
  }
}
```

Klax executes the command directly, without a shell. It writes one compact JSON
object followed by LF to standard input and waits synchronously for the command
to exit.

- Exit status 0 acknowledges the boundary.
- Failure to start, termination by signal, or a non-zero exit status fails the
  hook.
- A failed `turn.start` hook prevents backend invocation. Klax marks the
  durable turn `audit-start-failed` and shows a generic system error. It emits
  no `turn.finish` for that turn.
- A failed `turn.finish` hook does not change the completed turn result or
  prevent final delivery. Klax records and displays a generic durable system
  warning associated with the turn.
- Klax imposes no hook timeout or cancellation. A stuck hook intentionally
  blocks the turn. Operators may put their own timeout or asynchronous handoff
  inside the configured command.
- Standard output is outside the protocol. Standard error and exit details are
  written to the local klax log, not exposed to users.

Hooks are completely disabled unless their commands are explicitly configured.
Configuring only one boundary enables only that hook.

The user-visible error or warning caused by a failed hook is an internal
durable klax system event. It is not another `klax.audit/v1` event and is not
part of the audited turn snapshot.

## Envelope and compatibility

```json
{
  "schema": "klax.audit/v1",
  "event": "turn.start",
  "turn": {}
}
```

- `schema` is exactly `klax.audit/v1`.
- `event` is a namespaced event name matching
  `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`.
- `turn` is required for every `turn.*` event.
- Consumers reject unknown schema versions.
- Stream consumers ignore unknown v1 event names and unknown object
  properties. A command bound to one exact event may reject a different event.
- Adding another event family or additive optional fields does not require v2.
  Changing the meaning or required shape of an existing event does.

## Boundary ordering

`turn.start` is emitted after all of the following:

1. the inbound request and attachments are durable;
2. routing, session, backend, launch options, trigger processing, attachment
   materialization, prompt canonicalization, and internal validation are done;
3. the durable run fence and the backend transcript start cursor are recorded.

It is emitted before the backend process or backend turn is started.
The pre-run transcript cursor bounds the later binding search; it is not
`trace.raw.from_event`.

`turn.finish` is emitted after all of the following:

1. the backend call has returned;
2. klax has formed the final result, usage, and context snapshot;
3. the final delivery-prepared result and trace snapshot are complete.

It is emitted before klax performs its final delivery step. Streaming surfaces
and the durable web read model may already have shown intermediate or complete
backend content.

`turn.finish` attests completion of the backend computation and construction of
the final klax turn result. It does not attest that every internal terminal
queue or session-metadata write succeeded.

A process crash or failed start hook may leave a `turn.start` attempt without a
`turn.finish`. A backend turn that passed its durable run fence is never
re-run. Klax v1 does not durably retry audit hooks: an auditor detects a
missing or failed completion record as an accepted `turn.start` without an
acknowledged `turn.finish`.

## Turn identity

`turn_id` is deterministic and requires no additional stored identifier:

```text
identity_bytes = Go encoding/json of:
  ["klax.turn/v1", session_key, session_created, turn_seq]

turn_id = lowercase hex(SHA-256(identity_bytes)[0:20])
```

The text form is exactly 40 lowercase hexadecimal characters (160 bits).
Uniqueness is scoped to one uninterrupted klax state store. An aggregator that
combines independent installations keys records by its own
`(installation_id, turn_id)`.

`turn_seq` is the durable monotonically increasing turn sequence within this
session incarnation's queue.

## Snapshot monotonicity

`turn.finish` is a self-contained monotonic extension of the complete
`turn.start` snapshot. Every field present in `turn.start` is repeated with the
same value. Consumers may trust this as a producer invariant without comparing
the repeated fields.

`turn.finish` may only add:

- `finish_at`;
- `execution.backend_session_id`, if it was unknown at start;
- `result`;
- `trace`.

`execution.backend_session_id` is add-only. If it was present at start, it
cannot change. If the backend reports a conflicting session ID, klax keeps the
start value, logs the invariant violation, and omits trace whose coordinates
cannot be trusted.

## Turn object

The common shape is:

```json
{
  "turn_id": "7cf1b93af31ea98138ea42ec8c6893b3ad6ca537",
  "turn_seq": 42,
  "accepted_at": "2026-07-26T12:00:00.123456789Z",
  "start_at": "2026-07-26T12:00:01.234567890Z",
  "finish_at": "2026-07-26T12:00:08.345678901Z",
  "origin": {},
  "routing": {},
  "request": {},
  "execution": {},
  "result": {},
  "trace": {}
}
```

Times are UTC RFC 3339 strings with the available fractional precision.

- `accepted_at`: durable inbound acceptance.
- `start_at`: the `turn.start` boundary before its hook.
- `finish_at`: the `turn.finish` boundary before its hook.

`finish_at`, `result`, and `trace` are forbidden in `turn.start`.

## Origin

`origin` contains immutable transport-native coordinates captured at
acceptance:

```json
{
  "transport": "ym",
  "chat": {
    "id": "0/0/example-chat",
    "type": "group",
    "title": "Example group",
    "thread_id": "example-thread"
  },
  "message": {
    "id": "123456",
    "sent_at": "2026-07-26T11:59:59Z"
  },
  "sender": {
    "id": "example-user-id",
    "username": "user@example.org",
    "display_name": "Example User"
  }
}
```

- `transport` is the public audit transport name, currently `tg`, `max`, `vk`,
  `ym`, or `ui`. Internal routing may use a different prefix: MAX session keys,
  for example, retain their existing `mx:` prefix.
- `chat.id` is the transport-native chat ID without the klax prefix.
- `chat.type`, `chat.title`, and `chat.thread_id` are included when supplied by
  the transport.
- `message.id` is the transport-native message ID; UI uses its client nonce.
- `message.sent_at` is the transport-reported timestamp when available.
- sender fields are the transport-native ID, login/username, and display name
  known at acceptance.

`transport`, `chat`, `message`, and `sender` are present. Only `chat.id` is
required inside the nested objects; unavailable optional values are omitted.

## Routing

```json
{
  "session_key": "ym:0/0/example-chat#example-thread",
  "session_created": 8,
  "session_name": "work"
}
```

- `session_key` is the canonical klax routing key and may represent a
  transport chat or a cross-transport mapped user.
- `session_created` is an opaque monotonic identifier of this durable session
  incarnation; consumers must not interpret it as a timestamp.
- `session_name` is the user-visible name at the start boundary, when set.

## Request and attachments

```json
{
  "original_text": "@bot inspect the report",
  "effective_prompt": "inspect the report\n\nAttachments:\n- /tmp/klax-attach-123/report.pdf",
  "attachments": [
    {
      "name": "report.pdf",
      "path": "/absolute/klax/session/files/000042-01-report.pdf",
      "size": 38124,
      "sha256": "8f54d50b5849b3f8633ec46b012ca6631cfe1c48f5d421f79a7b544cab101db8"
    }
  ]
}
```

- `original_text` is the transport text before group-trigger removal and
  prompt canonicalization.
- `effective_prompt` is the exact prompt submitted to the backend, including
  temporary materialized attachment paths.
- `attachments` is omitted when empty and otherwise retains the stable inbound
  attachment order represented by the `NN` component of its durable filename.
- `name` is the run-view basename visible to the backend after collision
  disambiguation. It need not equal `filepath.Base(path)`, which is klax's
  prefixed durable stored filename.
- `path` is the absolute path to the durable session-store file, not the
  temporary path embedded in `effective_prompt`.
- `size` and `sha256` describe the durable file bytes.

Klax read the durable bytes while constructing the snapshot. The path is
normally still openable by the hook process, subject to operating-system
permissions, but concurrent session deletion is an accepted failure and klax
does not pin the file for the hook. Consumers must handle a missing path and
copy any bytes they need before the hook returns.

## Execution

```json
{
  "backend": "claude",
  "backend_session_id": "89c19fb7-690b-4bf1-85ef-4d68cdfd49df",
  "cwd": "/work/project",
  "model_requested": "sonnet",
  "effort": "high",
  "sandbox": "workspace-write",
  "tty": false,
  "append_system_prompt": "Answer in Russian."
}
```

- `backend` is the selected backend implementation.
- `backend_session_id` is the backend's session identity when known.
- `cwd` is the backend working directory.
- `model_requested`, `effort`, `sandbox`, and `append_system_prompt` are
  explicit launch overrides and are omitted when unset.
- `tty` says whether klax used its TTY integration.

The backend-reported effective model belongs in `result.model_used`.

## Result

`result` is required in `turn.finish` and forbidden in `turn.start`:

```json
{
  "status": "success",
  "output": {
    "text": "The report is valid.",
    "format": "markdown"
  },
  "model_used": "example-model",
  "tokens": {
    "input": 1520,
    "output": 240,
    "cache_read": 900,
    "cache_creation": 0
  },
  "context_after": {
    "used": 28410,
    "window": 200000
  },
  "elapsed_ms": {
    "queued": 1111,
    "start_hook": 24,
    "backend": 7031,
    "finalize": 155,
    "total": 8321
  }
}
```

`status` is `success`, `error`, or `aborted`.

`output`, when present, is the final answer prepared for delivery:

- `text` is the complete answer;
- `format` describes its markup, currently normally `markdown`.

It is not transport-specific chunking and does not assert delivery. It may
overlap the last assistant block in `trace.blocks`, but equality is not
guaranteed because output is the delivery-prepared result. Partial output may
be present for an error or aborted turn.

`error` is required for `error` and `aborted`:

```json
{
  "stage": "backend",
  "code": "backend-failed",
  "message": "backend process exited with status 1"
}
```

- `stage` is currently `klax` or `backend`.
- `code` is a stable machine-readable classification.
- `message` is diagnostic and must not be used for grouping.

Known v1 codes are `aborted`, `run-start-failed`, and `backend-failed`.
`audit-start-failed` is a durable queue reason, not a `turn.finish` result code,
because a denied start produces no finish event. Consumers display unknown
future v1 codes as generic errors.

`tokens` describes this backend turn. Unknown counters are omitted rather than
encoded as negative values.

`context_after` is the canonical session context snapshot after a successful
turn. It is distinct from per-turn input tokens.

`elapsed_ms` contains non-negative wall-clock durations:

- `queued`: acceptance to the start boundary;
- `start_hook`: the successful start hook invocation;
- `backend`: the backend call;
- `finalize`: backend return through persistence and trace construction to the
  finish boundary;
- `total`: exactly `queued + start_hook + backend + finalize`.

`total` is the sum of the listed phases, not an independently measured
wall-clock span.

The finish hook duration is not part of the event because the event is already
formed before that hook starts.

## Trace

`trace` is optional in `turn.finish` and forbidden in `turn.start`:

```json
{
  "blocks": [],
  "raw": {
    "path": "/absolute/backend/session/transcript.jsonl",
    "from_event": 128,
    "to_event": 141,
    "sha256": "f9b33c1a43604b9d8876e9753f6bc882ca8e8ebead541a7211f7e08250f8c914"
  }
}
```

Both representations cover the same backend turn. A completed successful turn
includes its final answer; an aborted or failed turn may not have one:

```text
trace.blocks =
  klax_normalize(
    bytes/records addressed by trace.raw[from_event:to_event),
    with normalizer state initialized at the slice boundary
  )
```

Raw is the canonical source. Blocks are derived from that exact snapshot, not
from an independent live/UI stream.

### Normalized blocks

`trace.blocks` is the same convenient, intentionally lossy representation that
klax's ordinary UI derives from the raw turn range. It contains no information
beyond that UI normalization. It can include the backend-recorded user prompt,
assistant text and normalized tool calls, compaction events, backend API error
rows, and the final assistant answer when one exists. Tool results are not
included.

```json
{
  "role": "assistant",
  "text": "Inspecting the file.",
  "tools": [
    {
      "name": "Read",
      "label": "📖 Read: report.pdf"
    }
  ],
  "time": "2026-07-26T12:00:05.123Z",
  "ctx_used": 28410,
  "ctx_window": 200000
}
```

- `role` is `user` for the backend-recorded prompt, `assistant` for assistant
  text and/or normalized tool calls, `tool` for compaction, or `system` with
  `kind:"error"` for a backend API error row.
- `text` is normalized text/Markdown.
- `tools` contains normalized tool calls. `name` is the stable canonical tool
  identifier. `label` is a short human-readable UI presentation whose exact
  formatting, width, punctuation, symbols, and language are unstable and must
  not be parsed.
- `kind` is `error` when applicable and otherwise omitted.
- `time`, `ctx_used`, and `ctx_window` are included when known.
- Empty optional values are omitted.

Internal read-model fields such as durable sequence, pending state, legacy
marker, event coordinate, record digest, prompt digest, backend, and session
are never serialized as public blocks.

Blocks are a helper, not forensic evidence. Backend-specific structure,
reasoning, full tool inputs/results, or a one-block-to-one-event mapping are not
promised. There are no per-block raw coordinates. Normalization starts with
empty state at `from_event`, so it may differ slightly from rendering the same
records as part of the whole session. Changes to the public block shape or
meaning are protocol changes.

### Raw range

- `path` is the absolute path to the exact backend JSONL transcript file.
- Klax read the transcript at snapshot construction. The path is normally still
  openable by the finish hook, but klax does not own backend retention and
  consumers must handle a missing path.
- `from_event` is the zero-based physical index of the backend user record
  durably and unambiguously bound to this klax turn.
- `to_event` is the exclusive physical index after the last complete record
  observed for the turn.
- Every complete physical record counts regardless of its type or embedded
  timestamp.
- Timestamps never define the range. Later physical records may contain earlier
  timestamps.
- A compaction boundary is an ordinary appended physical record.

The range begins exactly at the bound backend user record and includes every
following backend record through completion of this turn: the recorded prompt,
intermediate output, tool activity, compaction, usage/context data,
final-answer events when present, and other backend-specific records.
Backend records physically written before the bound user record, such as
session or turn setup and Codex `turn_context`, are outside the range by
construction.

`sha256` is the lowercase SHA-256 of the exact contiguous physical byte span:

```text
transcript bytes from byte_offset(from_event) to byte_offset(to_event)
```

It includes original JSON bytes, whitespace, LF terminators, and CR bytes when
the file uses CRLF. Klax must not parse/reserialize records, join normalized
record payloads, or normalize line endings to compute it.

The binding that supplies `from_event`, `to_event`, the byte range, its hash,
and `trace.blocks` are derived from one read of the transcript.

Klax neither embeds, copies, compresses, nor archives raw transcript bytes. A
consumer that needs independent retention must copy the addressed bytes before
acknowledging the finish hook. After hook success, klax does not guarantee
permanent file existence.

The whole trace is omitted if the backend user record cannot be bound
unambiguously to the klax turn, the backend session ID or transcript path is
unknown, the file cannot be read, `to_event < from_event`, the backend session
ID conflicts with the start snapshot, or the same snapshot cannot be normalized
into blocks. Trace enrichment failure does not change the backend result status.

## Complete `turn.start` example

```json
{
  "schema": "klax.audit/v1",
  "event": "turn.start",
  "turn": {
    "turn_id": "7cf1b93af31ea98138ea42ec8c6893b3ad6ca537",
    "turn_seq": 42,
    "accepted_at": "2026-07-26T12:00:00.123456789Z",
    "start_at": "2026-07-26T12:00:01.234567890Z",
    "origin": {
      "transport": "ym",
      "chat": {
        "id": "0/0/example-chat",
        "type": "group",
        "title": "Example group",
        "thread_id": "example-thread"
      },
      "message": {
        "id": "123456",
        "sent_at": "2026-07-26T11:59:59Z"
      },
      "sender": {
        "id": "example-user-id",
        "username": "user@example.org",
        "display_name": "Example User"
      }
    },
    "routing": {
      "session_key": "ym:0/0/example-chat#example-thread",
      "session_created": 8,
      "session_name": "work"
    },
    "request": {
      "original_text": "@bot inspect the report",
      "effective_prompt": "inspect the report",
      "attachments": [
        {
          "name": "report.pdf",
          "path": "/absolute/klax/session/files/000042-01-report.pdf",
          "size": 38124,
          "sha256": "8f54d50b5849b3f8633ec46b012ca6631cfe1c48f5d421f79a7b544cab101db8"
        }
      ]
    },
    "execution": {
      "backend": "claude",
      "backend_session_id": "89c19fb7-690b-4bf1-85ef-4d68cdfd49df",
      "cwd": "/work/project",
      "model_requested": "sonnet",
      "effort": "high",
      "tty": false
    }
  }
}
```

## Complete `turn.finish` example

```json
{
  "schema": "klax.audit/v1",
  "event": "turn.finish",
  "turn": {
    "turn_id": "7cf1b93af31ea98138ea42ec8c6893b3ad6ca537",
    "turn_seq": 42,
    "accepted_at": "2026-07-26T12:00:00.123456789Z",
    "start_at": "2026-07-26T12:00:01.234567890Z",
    "finish_at": "2026-07-26T12:00:08.345678901Z",
    "origin": {
      "transport": "ym",
      "chat": {
        "id": "0/0/example-chat",
        "type": "group",
        "title": "Example group",
        "thread_id": "example-thread"
      },
      "message": {
        "id": "123456",
        "sent_at": "2026-07-26T11:59:59Z"
      },
      "sender": {
        "id": "example-user-id",
        "username": "user@example.org",
        "display_name": "Example User"
      }
    },
    "routing": {
      "session_key": "ym:0/0/example-chat#example-thread",
      "session_created": 8,
      "session_name": "work"
    },
    "request": {
      "original_text": "@bot inspect the report",
      "effective_prompt": "inspect the report",
      "attachments": [
        {
          "name": "report.pdf",
          "path": "/absolute/klax/session/files/000042-01-report.pdf",
          "size": 38124,
          "sha256": "8f54d50b5849b3f8633ec46b012ca6631cfe1c48f5d421f79a7b544cab101db8"
        }
      ]
    },
    "execution": {
      "backend": "claude",
      "backend_session_id": "89c19fb7-690b-4bf1-85ef-4d68cdfd49df",
      "cwd": "/work/project",
      "model_requested": "sonnet",
      "effort": "high",
      "tty": false
    },
    "result": {
      "status": "success",
      "output": {
        "text": "The report is valid.",
        "format": "markdown"
      },
      "model_used": "example-model",
      "tokens": {
        "input": 1520,
        "output": 240,
        "cache_read": 900
      },
      "context_after": {
        "used": 28410,
        "window": 200000
      },
      "elapsed_ms": {
        "queued": 1111,
        "start_hook": 24,
        "backend": 7031,
        "finalize": 155,
        "total": 8321
      }
    },
    "trace": {
      "blocks": [
        {
          "role": "user",
          "text": "inspect the report",
          "time": "2026-07-26T12:00:01.300Z"
        },
        {
          "role": "assistant",
          "text": "Inspecting the report.",
          "tools": [
            {
              "name": "Read",
              "label": "📖 Read: report.pdf"
            }
          ],
          "time": "2026-07-26T12:00:03.100Z"
        },
        {
          "role": "tool",
          "text": "🗜 Compaction: auto",
          "time": "2026-07-26T12:00:04.200Z"
        },
        {
          "role": "assistant",
          "text": "The report is valid.",
          "time": "2026-07-26T12:00:07.900Z",
          "ctx_used": 28410,
          "ctx_window": 200000
        }
      ],
      "raw": {
        "path": "/absolute/backend/session/transcript.jsonl",
        "from_event": 128,
        "to_event": 141,
        "sha256": "f9b33c1a43604b9d8876e9753f6bc882ca8e8ebead541a7211f7e08250f8c914"
      }
    }
  }
}
```
