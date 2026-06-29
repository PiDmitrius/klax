# klax UI refactor — server-authoritative turn model + modular SPA (v3 — CONVERGED)

## Why
The SPA reconstructs each turn's state (queued / running / done + per-turn ordering)
from event *timing* across ~9 semi-authoritative client stores. The durable
`queue.jsonl ⋈ transcript` already **is** that state. Every fix so far added another
client cache; the 2 High + 2 Medium bugs all trace to disagreements between these
stores (independent pico review 019f0fa0). This refactor moves turn-state truth to the
server, renders it directly, and splits the monolith `index.html` into ES modules.

> Converged with independent reviewer pico 019f0fa0 over two amendment rounds: turn-shaped
> pagination, reload watermark + mandatory canonical-content `Block.id` dedup, per-turn +
> per-block `eventSeq` unread, `/api/send → {seq,state}` + idempotent `bindNonce`,
> terminal-state precedence, absolute legacy markerless ids, the ES-module static handler,
> module-ownership cuts, and a 6-scenario browser test harness.

## Invariants — MUST NOT regress
- Per-turn ordering: an answer renders under *its own* message, never after a later one.
- Per-turn indicator: `enq`→ dim "в очереди · N"; `run`→ animated dots + ✕ abort; `done`→ none; `err`→ error bubble.
- Full reload reproduces the exact pre-reload state **including queued messages**.
- Attachments: paste / drop / paperclip chips, image thumbnails inline, non-images inert via `/api/file` capability refs.
- Mount-aware BASE (served behind a path-stripping proxy under `/klax/`).
- Background-tab unread badge + "новые сообщения" divider.
- Optimistic echo on send (instant local bubble).
- Messenger (tg/max/vk) paths, sealed-ref serving, auth, durable-queue backend semantics — **untouched**.

## A. Server protocol — the single source of truth

### A1. `/api/transcript?session&before&limit` → turn-shaped page
Returns `{turns: [Row], more, offset, watermark}`.
- `Row` user: `{seq:int64, role:"user", text, time, state, eventSeq?, blocks:[Block]}` — `state ∈ {enq,run,done,err}`; `eventSeq` is the accept event's cursor (for unread, B3); `blocks` are this turn's answer in order.
- `Block`: `{id, role:"assistant"|"tool"|"system", text?, tools?, kind?, eventSeq?}` — `id` is a STABLE id `"<seq>:<shorthash(role,text,tools)>"` hashed from **canonical** content **before** per-response `/api/file?ref=` rewriting (capability refs change every render and would break dedup), so the same block hashes identically in the transcript and the live event (the dedup key, A3); `eventSeq` is the poll cursor of the event that delivered it live (absent for a transcript-only block until a matching live event attaches it).
- **Pagination is by TURN, not flat item**: `before`/`offset`/`limit` count user turns. Older pages never include pending rows. Latest-page (`before==0`) pending turns are appended **after** transcript turns, sorted by `seq`.
- `watermark`: klax's event cursor at read time (A3).

**Build**: `history.Load` → group transcript into user turns (a user row + its following assistant/tool/system blocks). For each, `marker ⋈ queue.jsonl` → assign `seq` + `state` by the **precedence table** below. Then on `before==0` append queue turns absent from the page (`enq`, and `run` not-yet-flushed) as user rows with `seq`+`state` (empty `blocks`). → fixes **High#1** (running transcript turn carries `seq`+`state=run`) and **High#2** (`run` not flushed surfaced).

**Terminal-state precedence** (queue `Last` ⋈ transcript), avoids a stale `run` forever after a missed `MarkDone`:
| queue `Last` | resolved `state` |
| --- | --- |
| `enq` | `enq` |
| `run` | `run`, **unless** transcript shows a terminal assistant answer for the turn AND the session is not busy → then `done` |
| `done` | `done` |
| `err` | `err` |
| marker not in queue (legacy) | `done` |

**Legacy markerless transcript rows**: a user row with no klax-turn marker is a completed legacy turn — render it `state:"done"` with a synthetic id from its **absolute transcript turn ordinal**: `seq = -(start + i + 1)` (stable across pages — never page-local, which would collide; never a real durable seq ≥ 1; never live-routed).

### A2. Events carry state (`uiEvent`) + are monotonic
- `user`: `{seq, role:"user", state:"enq", text, nonce, time, eventSeq}` — emitted on durable accept, **including accept-during-drain**. `eventSeq` = this event's cursor (for unread). (Fixes the drain-accept Medium.)
- `turn_start`: `{seq, state:"run"}`
- `progress`: `{seq, block:{id, role, text|tools}, eventSeq}`
- `final`: `{seq, state:"done", block:{id, …}, eventSeq}`
- `error`: `{seq, state:"err", block:{id, …}, eventSeq}` — err renders as a block, with an id

State is **monotonic**: `enq → run → done|err`; a terminal state wins; a later non-terminal transition for a terminal turn is ignored. Every event carries its monotonic poll `seq` (already present) for dedup.

### A3. Reload watermark + idempotency (anti-duplication)
The `watermark` alone is NOT sufficient: the transcript read and the event cursor are not a synchronized snapshot, so a block flushed *during* the reload read can appear in BOTH the reloaded transcript AND a later poll event. Therefore **dedup by stable `Block.id` is mandatory** (not a fallback): `appendBlock` skips a block whose `id` already exists, and if the incoming live copy carries an `eventSeq`, it attaches that to the existing block so unread (B3) still sees it. The `watermark` (set `cursor = lastSeq = watermark` after reload) plus the existing `seq ≤ lastSeq` event-drop handle ordering and the common case; `Block.id` closes the reload-read/poll race window. A snapshot barrier (blocking event emission during the transcript read) is the alternative but is more invasive — stable block ids are simpler. → resolves run-straddles-reload duplication at the protocol level (harness scenarios 1 & 4).

### A4. `/api/send` returns `{seq, state}` (not 204)
UI sends get `200/202 {seq, state}` after durable accept (incl. accept-during-drain → `{seq, state:"enq"}`). The sender binds its optimistic row to the real `seq` **immediately on the POST response**; the poll `user` event remains the cross-tab/cross-channel broadcast and an idempotent backstop.

## B. Client: model + render (replaces the state machine)

### B1. `model.js` — one ordered array per session, pure data
`model[created] = [Turn]`, `Turn = {seq, role:"user", text, time, state, eventSeq?, blocks:[Block], nonce?}`. Ops, all **idempotent + monotonic**:
- `reconcile(created, rows, watermark)` — replace from a reload.
- `upsertUser(created, {seq,nonce,text,time,eventSeq}, state)` — from the `user` event / send response.
- `setState(created, seq, state)` — monotonic; terminal wins.
- `appendBlock(created, seq, block)` — dedup by `block.id` (existing id wins; attach a live `eventSeq` onto the existing block); otherwise merge consecutive same-role and append.
- `bindNonce(created, nonce, seq, state)` — **3 cases**: optimistic nonce row exists → promote to `seq`; else seq row already exists → drop the nonce/ignore (no dup); else → upsert a new user row.
No `runningTurn` / `doneTurns` / `queuedTurns` / `tmpTurn`.

### B2. `render.js` — model → DOM (owns all turn DOM)
One container `<div class="turn" data-seq>` per user turn; its `blocks` render **inside**
it (consecutive same-role merge *within the container only* → cross-turn merge structurally
impossible, `breakMerge` deleted). The tail indicator is computed from `Turn.state`.
Out-of-order routing = look up the container by `seq`, append inside (no array-index
surgery → `insertAnswer(freshBefore)` deleted). Owns `data-seq`, state indicators, block
DOM, and the unread divider placement. Re-render is selection-safe (defer during a text
selection, as today).

### B3. Unread by event-bearing blocks
Per tab: `lastReadSeq` = the max event `seq` seen while viewing. A turn or block is unread
if its `eventSeq > lastReadSeq` — user turns carry an accept `eventSeq`, so a queued `enq`
row with no blocks still participates. Badge counts unread turns/blocks; the "новые
сообщения" divider sits before the first turn (DOM order) carrying an unread turn/block.
Seq-based, monotonic — no array-index shifting (`readMark` surgery deleted). A full reload
**resets** each tab's baseline to its reload `watermark` (pre-restart event seqs are
unknowable); live events after that compute unread normally. An out-of-order unread block
in an above-divider turn moves the divider up to that turn (accepted, correct count).

### B4. `compose.js` — optimistic by nonce
The optimistic bubble is a Turn keyed by **nonce**, `state:"sending"`. It binds on the
**`/api/send` response** (`bindNonce`), with the poll `user` event as an idempotent
backstop. Negative `tmpTurn` routing deleted; a nonce never participates in answer
routing. Owns draft text, staged files, **blob-URL lifecycle** (revoke on bind/replace/
remove → closes the Low leak), autoGrow, paste/drop. Talks to the controller via
`onOptimisticSend(nonce,…)` / `onAccepted(nonce,seq,state)` callbacks — never reaches into
render internals.

## C. Replaces-what — nothing is lost
| deleted | replaced by |
| --- | --- |
| `tmpTurn` negatives | optimistic Turn by `nonce`; `bindNonce` (A4 send response + user event) |
| `runningTurn` | `Turn.state === "run"` |
| `doneTurns` | `Turn.state === "done"` |
| `queuedTurns` | `Turn.state === "enq"` |
| `renderedPending` + trailing `syncTyping` | per-turn dots from `Turn.state` (every turn has a state) |
| `insertAnswer(arr, e, freshBefore)` | `appendBlock` into the turn container found by `seq` |
| `readMark` array-index shifting | `lastReadSeq` (event-seq based, B3) |
| `breakMerge` | merge only within a turn container |
| `it.turn>0 && running!==undefined` queued guess | `Turn.state === "enq"` (authoritative) |
| `busy` set (per-tab dot) | derived: tab busy if any turn is `enq`/`run`, or `/api/sessions` coarse flag |

## D. Modular SPA (native ES modules, no bundler)
`cmd/klax/ui_static/` embedded via **`//go:embed ui_static`** (whole dir) + `fs.Sub`:
- `index.html` — shell only: gate/auth + composer markup, `<link rel=stylesheet href=./app.css>`, `<script type=module src=./app.js>`.
- `app.css` — theme + layout + components.
- `base.js` (BASE/`apiHref`/token/`fetch` only) · `markdown.js` · `model.js` (pure data, no DOM/fetch) · `render.js` (turn DOM + indicators + divider) · `scroll.js` (stickiness, scroll-bottom, unread-jump, selection-safe defer) · `events.js` (poll loop + event→model patches; **does not own tabs**) · `sessions.js`/`tabs.js` (`/api/sessions`, tab select, busy counts, session-model invalidation; may hold the settings modal initially) · `compose.js` (B4) · `app.js` (wiring only).

**Static asset handler** (new): the current `handleSPA` 404s every path but `/`
(`cmd/klax/ui_spa.go`, `cmd/klax/ui.go`). Add a static handler serving `/app.js`,
`/model.js`, `/app.css`, … from the embedded `ui_static` FS: `.js` → `text/javascript;
charset=utf-8`, `.css` → `text/css; charset=utf-8`; **no auth** (same as the current SPA/
emoji); `/api/*` must **never** fall through to `index.html`; relative module imports
(`./x.js`) verified under the real external `/klax/` mount.

## E. Steps — each ends with `pico-codex -s` convergence
1. **Server protocol**: turn-shaped `/api/transcript` rows + `watermark` + `Block.eventSeq` + event `state` fields + `/api/send → {seq,state}`. Go tests: marker join; `enq`; `run`; `run` not flushed; `err`; legacy markerless; terminal-state precedence; turn-based pagination; watermark monotonicity. **The watermark/block-id contract is part of THIS step, not a later check.**
2. **Client core** (`model.js` + `render.js` + `events.js` + `scroll.js`) against the new protocol — prove per-turn ordering, state dots, reload, optimistic-by-nonce. New code, **not** a mechanical split of the old monolith.
3. **Port** `compose.js` / `tabs.js`/`sessions.js` / `markdown.js` / `base.js`; `embed.FS` static handler; `index.html` shell + `app.css`.
4. **Delete** the old monolith state machine; final pico convergence + live verify; deploy on the owner's word.

### Test harness (before deletion)
A browser/DOM harness (Playwright, or jsdom if lighter) driving the new client against a
**fake `/api/transcript` + fake poll stream** — the level that would have caught the prior
bugs. Required scenarios:
1. Reload while A is `run`, B is `enq` → A's `final` routes under A (not after B).
2. `run` not flushed to transcript still renders after reload.
3. Accept-during-drain send shows `enq`, not a spinning unknown state.
4. A `final` present in both the reloaded transcript and a poll event does not double-render.
5. Own-nonce `user` event after reload neither duplicates nor disappears the row.
6. Background tab gets an out-of-order `final` above the divider and still flags unread.

## F. Risks / checks
- ES-module MIME + relative paths under the mount proxy — verify a real `/klax/` load.
- Server + client ship in one binary → no protocol skew.
- Keep sealed-ref `/api/file`, auth, messenger untouched.
- Attachments through the new model: optimistic blob thumbnails by nonce; durable `/api/file` thumbnails on reload; revoke blob URLs on bind/replace.
- run-straddles-reload is fixed by A3 (watermark) — scenario 1 + 4 in the harness prove it.
- Outbound files on reload: served AS-IS from disk — `rewriteOutboundForUI` re-resolves the link from cwd; if the source path is gone the link degrades to a label (or `/api/file` 404s). By owner decision (2026-06-29) the engine adds NO persist-mapping / recovery logic: it serves disk content + the conditions, the agent and user own their workflow. So the pico "outbound reload mapping" Medium is won't-fix by design, not a Step-2 blocker.
