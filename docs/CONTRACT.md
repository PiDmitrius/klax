# klax contract

The invariants whose owner spans more than one file. They are the ones that get re-derived in a
second place and drift; an invariant that governs exactly one file lives at the top of that file,
not here.

How to read an entry: a fact, the single place that owns it, and what owning it forbids. Entries
are normative — code that deviates is a defect, not an exception. Each entry names the code that
expresses it, so an entry can be checked and can be proven stale.

## Turn lifecycle

1. **A turn's state is durable and klax-owned.** `queue.jsonl` records `enq | run | done | err`
   (+ a classified `reason` for `err`) through `sessfiles.Store` (`internal/sessfiles/queue.go`);
   `resolvedTurnState` (`cmd/klax/readmodel.go`) is the only place that turns those records into a
   displayed state. Terminality is that state, and `explainedByTranscript`
   (`cmd/klax/readmodel.go`) is the only place a transcript row is allowed to stand in for the
   turn's terminal block — for the reason `backend-failed` alone, and only when the row is the
   turn's last. Forbidden: inferring "this turn is over" from the content, role or position of a
   backend transcript row.

2. **The reason a turn failed is classified by klax; the detail text belongs to the backend.**
   `turnErrorReason` (`cmd/klax/error_reason.go`) maps a run failure onto the fixed set of reasons
   stored in the queue. The exact message lives only in the backend's own transcript and is parsed
   in one place per backend (`runner.ParseCodexTerminalError` for codex). Forbidden: persisting a
   copy of the message into the queue, and forbidden: a second formatter for it — the messenger and
   the reconstructed UI render the same parse.

## Delivery

3. **`...` means the turn is running, and nothing else.** It is appended in one place
   (`withProgressEllipsis`, `cmd/klax/helpers.go`) on the progress path. Every terminal outcome —
   answer, abort, failure — ends with its own explicit marker. Forbidden: a terminal message that
   carries the working marker, in any format.

4. **Progress, success and failure share one chunker.** `formatLogChunks` (`cmd/klax/helpers.go`)
   packs the log at item granularity for all three, so a terminal chain is never shorter than the
   progress chain it replaces and no chain message survives unedited. Forbidden: splitting a
   terminal message by a different rule than the progress message it overwrites.

## Web UI

5. **Live delivery and reload are one path.** The client long-polls `/api/tail` with per-session
   durable cursors; `buildReadModel` (`cmd/klax/readmodel.go`) rebuilds rows by joining
   `queue.jsonl` with the backend transcript. Forbidden: an in-memory ring, a global cursor, or a
   separate reload path.

6. **The unread axis is the durable `(turn_seq, block)`,** encoded `pos = turn * POS_MULT + block`
   (`cmd/klax/ui_static/render.js`). Forbidden: a second position scheme, and forbidden: re-zeroing
   an axis the server did not move.

7. **Live animation has one serialization point.** Every live trigger goes through `commitLive`
   guarded by `liveBusy` (`cmd/klax/ui_static/app.js`), which accumulates instead of stacking.
   Forbidden: rendering live outside it.

8. **Dropdowns are a custom component** (`cmd/klax/ui_static/tabs.js`). A native `<select>` hides
   its open state from JS, so a background refresh drops it.

9. **A row's visual class is computed once.** `blockCls` (`cmd/klax/ui_static/render.js`) maps a
   row to its class for standalone rows and grouped blocks alike, and the same class is the
   grouping discriminator, so a group can never mix classes. Forbidden: a second class mapping at
   a call site.

10. **Compaction is an ordinary tool row.** `internal/history/history.go` emits it as
    `Role: "tool"` for both backends, on the live and the reload path alike. Forbidden: a dedicated
    compaction block type or a special case in the renderer.

11. **The context line is emitted whenever `ctx_used > 0`,** with or without a known window
    (`cmd/klax/readmodel.go`), and it is the turn's last element. Forbidden: gating it on the
    window being known.

## Session identity

12. **A session is `(sessKey, Created)`.** `Created` advances from a store-global high-water and is
    never reused (`internal/session/session.go`); runners are keyed by it (`runnerKey`,
    `cmd/klax/daemon.go`) and a queued message binds to it at enqueue. Forbidden: resolving a
    running turn through the chat's *active* session.

13. **Backend and CWD freeze once a session has messages.** Both surfaces refuse the change under
    the store lock (`cmd/klax/commands.go`, `cmd/klax/ui_settings.go`), which is what keeps a
    turn's file-link keys stable. Everything else that affects a run is only busy-gated.

## Files

14. **A link serves the snapshot taken when the answer was delivered,** never the live path. The
    index and its keys are defined once at the top of `internal/sessfiles/links.go`; that header is
    the owner of the rule, this entry only points at it.

## Transports

15. **Addressing form is decided by the transport's own predicate,** not re-derived at call sites:
    `ym.IsLogin` / `ym.IsGroup` (`internal/ym/ym.go`) decide private vs group from `chat.type`.

16. **Credentials are redacted at the log sink.** `redactingWriter` (`cmd/klax/daemon.go`) is the
    single chokepoint every log line passes through. Forbidden: redacting at a call site.

## Public surfaces

17. **`docs/audit-v1.md` + `docs/audit-v1.schema.json` are the external contract** for the audit
    stream. A turn's terminal outcome is carried by `result.status` and `result.error`; blocks are
    a rendering helper, not forensic evidence. Forbidden: changing the meaning or required shape of
    an existing event inside v1.
