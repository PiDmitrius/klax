# klax — приём картинок/файлов в UI + durable queue + outbound (design contract **v4**)

Status: v4, 2026-06-28, after pico-codex review rounds 1+2+3+4. Round 3 converged to 3 must-fix blockers (turn_marker, close-lock ordering, turn_seq on all turn-scoped events) — all applied; **round 4 verdict: READY** (no hard blockers; only minor impl hygiene + tests remain, folded below). Review traction: 17→14→3→0 hard blockers. One PR (dev→main), commits = layers.
Repo: `/home/claw/work/klax` (Go; Telegram/MAX/VK ↔ Claude/Codex CLI + web UI).
Tags: `[fixN]` = round-1 finding N; `[r2.N]` = round-2; `[r3.N]` = round-3.

## 0. Context / key finding

- Web UI mirrors messenger turns and lets the user send. **Server inbound already exists**: `handleSend` parses `multipart/form-data` `files[]`; whole-body cap 64 MiB. Frontend `send()` is JSON-only.
- Today: `buildTurnPrompt` writes bytes to `os.MkdirTemp("klax-attach-*")`, folds paths into the prompt, `defer RemoveAll`.

## 1. Storage layout

- `~/.local/share/klax/sessions/<keydir>/<created>/` (0700).
- `<keydir>` = `base64url(rawSessionKey)` (lossless ⇒ truly injective) `[r2.9]`, optionally prefixed with a sanitized hint for human eyes (`<hint>--<b64url>`). (A short hash is NOT injective — rejected.)
- `<created>` = `Session.Created` (per-session identity; one chat holds many tab-sessions; `sess.ID` unsuitable — empty pre-first-run, mutable).
- Inside: `files/` + `queue.jsonl`.
- **Stored file name = `<turn_seq>-<NN>-name.ext`** `[r2.4]`: `turn_seq` = the message's monotonic turn id (§3), `NN` = per-message file index (so several files in one message never collide, even duplicate pasted names). Real names + extensions, sanitized. `enq` stores the explicit `files[]` list — turn identity is NEVER inferred from a filename.
- File writes use `O_CREATE|O_EXCL`, retry on `EEXIST` `[fix6]`; sequence allocation happens under the **durable-store lock** (§3), not a caller convention.
- Pasted images → `pasted-N.png`. No per-message subdir.
- STATUS: `internal/sessfiles` partly DONE (layout/Materialize/Remove/Sanitize, tested); needs `[r2.4]` (turn_seq-NN names + explicit files[]), `[r2.9]` (base64url key), `[fix6]` (O_EXCL + lock-driven seq).

## 2. Agent-facing path (run-view)

- Agent NEVER sees the durable path. `buildTurnPrompt` materializes a clean `/tmp/klax-attach-*/name` by **copy** (not symlink — `realpath` leaks the internal path; not hardlink — `/tmp` tmpfs ⇒ `EXDEV`). `defer RemoveAll`; durable copy persists. Within-turn clash → `name-2.ext`.

## 3. Durable queue (B) + persistent inbound log + crash recovery

- **Two locks `[r2.5]`**: `sr.mu` guards ONLY runner state + the in-memory queue; a separate **durable-store mutex** guards disk seq allocation, file writes, `queue.jsonl` append/checkpoint, and tombstone. Ordering: never hold `sr.mu` during disk I/O; if both needed, durable-lock first, short `sr.mu` second.
- Records carry a **monotonic per-session `turn_seq`** (under the durable-store lock) `[fix7][r2.4]`: `enq(turn_seq, nonce, text, files[], ts, turn_marker)` → `run(turn_seq)` → `done(turn_seq)`/`err(turn_seq, reason)`.
- **Turn correlation marker `[r2.1][r3.1]`**: `turn_marker` = an opaque token klax generates at `enq` and **injects into the prompt** as an unobtrusive literal (e.g. a trailing `<!-- klax-turn:<token> -->` line the agent ignores). The backend records it verbatim in its user turn; `history.Load` extracts the token by regex and **strips it from the displayed text**. Everything keys off this literal token — NOT a hash of the sent prompt (the prompt carries random run-view paths and the backend may normalize the text, so a hash is fragile — `r3.1`) and NEVER off ordinal user-turn position (Claude injects background-task user messages — `runner.go:849`; a turn can `run` then fail before recording a user turn). `[r4]` Renderers strip any `<!-- klax-turn:… -->` marker from **all** displayed text (user/assistant/final) in case the agent echoes it; extraction matches **only** tokens that exist in the queue log, and never consumes an unmarked backend user turn. Tests must cover marker extraction across Claude string-content, Claude text-block content, Codex `user_message`, compact/summary records, and background-task user messages.
- **Acceptance fsync ordering `[fix4][r2.13]`**: write each file via temp in `files/`, `fsync(file)`, atomic `rename`, `fsync(files/ dir)`; **then** append `enq` + `fsync(queue.jsonl)`. Exact order is **durable files → enq fsynced → in-memory/reader state updated → HTTP ack → UI emit**. (Emit before durable append would resurrect ghost messages.)
- **Replay on startup, by `turn_seq`, reconciled against the transcript `[fix2][r2.7]`**:
  - `enq` without `run` → never reached the backend → re-enqueue (safe).
  - `run` without terminal → find the transcript user turn by `turn_marker`: a completed assistant span after it → recover `done`; only the marked user turn / partial items → `err:interrupted` (surface to user, do NOT auto-rerun — the backend may have run side effects); no marked turn in the transcript → `err:start-failed`.
  - terminal present → skip. Missing/short referenced file → corrupt → visible error, never run without files `[fix4]`. Session absent from `sessions.json` → handled by §8.
- **Checkpoint `[fix7]`**: periodic `ckpt(turn_seq=N)` = all `turn_seq≤N` terminal; replay scans only `turn_seq>N`.
- In-mem `sr.queue` holds **references** (turn_seq + stored file names), not bytes.
- **`enq` records never deleted** (only terminal-marked) ⇒ `queue.jsonl` is the **session-lifetime** `[fix8]` inbound log.
- RISKIEST layer: preserve queue invariants (bind-at-enqueue; one runner/session; `/switch`/`/new` mid-run don't redirect). Tested hardest.

## 4. Client = server-authoritative + per-turn render model

- The durable queue makes the **server** the owner of inbound truth. DELETE from `index.html`: localStorage `pending` (`save/loadPending`) + the `loadHistory` pending↔transcript reconciliation. Optional transient in-memory optimistic bubble, deduped by `nonce`. No 503-rollback (accept-during-drain).
- **Ack invariant `[fix15][r2.13]`**: ack = "`enq` fsynced AND history-visible". Reload renders **every acked `enq`**, including queued-not-yet-run (with files, marked queued). Tests: ack→kill before poll-event→restart→reload shows bubble+files marked queued; AND kill-after-emit-before-ack must NOT leave a ghost.
- **Per-turn render model (the payoff of the durable queue):** the log is a list of **turns** `{user-msg, answer-slot[], state}`, NOT a flat stream. Each turn's answer lands in ITS OWN slot directly under its user message → an answer can never be torn from its message (the messenger append-only problem disappears; we control the DOM). A queued message is drawn immediately with its slot + a three-dots placeholder = explicit "answers land here".
  - **Live routing `[r2.1][r3.3]`**: `uiEvent` gains `turn_seq`, and **every turn-scoped event carries it** — `user`, `turn_start`, `progress`, `final`, run-error, `aborted`, `closed`, in-run `compact` — so the client routes each to the correct turn's slot. Global `notice`/`sessions` snapshots stay unscoped. Replay/history terminal events drive the same per-turn state transitions as live.
  - Slot states: `queued` (dim dots) → `running` (active dots) → `done` (answer) → `aborted` (`/abort` dropped the queue → "отменено", not eternal dots; signal from the durable queue).
  - `syncTyping` generalizes from one trailing indicator to per-turn slots. The existing `breakMerge`/`turn_start` handling is the seed.
  - Scroll: a running turn's slot grows above later queued messages (normal chat behaviour); existing `stick`/`nearBottom` handles it.

## 5. Reload history = transcript-ordered, marker-correlated merge `[fix3][r2.1][r2.10]`

- Walk the backend transcript (already serialized). For each backend **user** turn, extract its `turn_marker`: if it matches a klax `run(turn_seq)`, render the klax inbound-log entry (clean text + file thumbnails) in its place; assistant/tool turns ← transcript as-is. Backend user turns with no marker (background injections, legacy/pre-feature) are rendered as plain text and do NOT consume a turn slot.
- Queued-but-not-run `enq`s (no `run`, no transcript turn) → appended after the last transcript turn, in `turn_seq` order, marked queued.
- Identical structure to the live per-turn model (§4) → live and reload render the same.
- **Pagination = a frozen snapshot cursor** `{snapshotTranscriptLen, snapshotMaxTurnSeq, beforeMergedIndex}` `[r2.10]`; page only within the snapshot (no single int offset — it can't index a merged, growing list).
- `handleTranscript` becomes this merge endpoint.

## 6. Security / serving — threat model FINAL

**Owner's decision (final):** we trust the agent. The agent runs with the user's tools and **network egress** (it can `curl`/`scp`/DNS-exfil any file to any third party). **Access to the agent is access to everything**; the user accepts this consciously when running klax. Therefore file-serving adds **zero** to the exfiltration surface — confinement here is NOT and cannot be a barrier against the (trusted) agent. It exists only to:
  1. **bound a leaked UI token / client forgery** — keep `/api/file` from being a broader arbitrary-read than what already appears in the conversation (a sniffed token shouldn't become whole-filesystem read);
  2. **stability** (immutable snapshots).
No outbound size cap, ever (the agent has unlimited egress regardless).

- `ref = base64url(AES-GCM(ephemeral_key, payload))`; ephemeral per-process key (restart → UI re-reads on epoch change → server re-mints).
- **The ref IS the capability `[r2.2]`**: `GET /api/file?ref=` takes NO bearer header (an `<img src>` cannot send one) — the sealed, scoped, short-`exp` ref is itself the auth (signed-URL model). **Capability hygiene `[r3.4]`**: very short `exp`; mint **fresh** refs on every history response (never persist a ref into a transcript/log); file + SPA responses set `Cache-Control: no-store` and `Referrer-Policy: no-referrer`; klax must not log query strings. (Alternative if we ever want owner re-check on every fetch: HttpOnly SameSite cookie. Not v1.)
- **Structured payload `[fix11]`**: `{session_key, created, path, exp, content_type}`.
- **Serve check `[r2.11]`**: decode+verify ref → `exp` valid → `store.Get(session_key, created)` exists and is not closing → path inside the immutable store → stream with `content_type`. A closed/tombstoned session's refs return 404/410 even before sweep.
- **Minting server-side only**, from agent output (outbound) or klax's own `files/` (inbound). Outbound pickup is confined to the session `cwd` (the agent's working dir) after `EvalSymlinks`; the served bytes live in the durable `files/` store (the snapshot). The client NEVER requests a mint.

## 7. Outbound (agent → user)

- Agent writes a file **in its `cwd`** (where it already works) and links it in its markdown answer. No `$KLAX_OUTBOX`: cwd-pickup is the model (outbox-only was rejected, and a dedicated drop dir is redundant — the snapshot-copy gives stability).
- **Strict href grammar `[fix14]`** (a parse boundary): accept only links/images whose href, after exactly one URL-decode, is a local path under root; reject `file://`, `~`, NUL/control chars, fragments; cap count + total size; dedupe by canonical opened-file identity.
- **Outbound finalization, before `done` `[r2.12]`**: at answer-parse, snapshot-**copy** each accepted file into the session's durable `files/` (immutable; stable across agent rewrite/delete + reload; no serve-time TOCTOU), mint a ref, rewrite the link to it. A failed snapshot (size/gone/perm/disk) → replace that link with a visible inline error, never a dead URL; `done` carries an outbound status list.
- UI: `inline()`/`md()` embed `<img>` (currently none) + same-origin file URLs.
- **Messengers deferred** (later PR; a local-path link in a messenger answer degrades to text). `klax send` CLI rejected.

## 8. Cleanup — crash-atomic close `[fix8][fix9][r2.6]`

- close/`/nuke` is a persisted state machine with a strict lock discipline `[r3.2]` — **never wait for a runner while holding the durable-store lock** (else the runner can't append its own terminal `done/err` + outbound snapshot before exit → deadlock): (1) durable lock → append+fsync `closing`, reject new enqueues, release; (2) brief `sr.mu` → set `closing`, capture cancel, release; (3) cancel the active run and **wait for the runner to exit holding NO durable lock**; (4) durable lock → append terminal close records, save `sessions.json` (marked deleted) via atomic-rename+fsync, then `RemoveAll(sessions/<keydir>/<created>/)`.
- Startup handles each state deterministically: a visible-closing session finishes removal; a session absent from `sessions.json` with an orphan dir is removed/archived; a `run` under a closing tombstone becomes `err:closed` (not generic `interrupted`).
- "session-lifetime", not "permanent": close/nuke deletes the log+files (acceptable — the session is gone from the UI). TTL sweep at daemon start: dirs older than N days or absent from `sessions.json`; a live session's dir is never swept.

## 9. Inbound UI (index.html) + streaming intake

- Entry points: Ctrl+V · drag-drop · paperclip (left of `#cbar`; textarea left-pad 14→~48; hidden `<input type=file multiple>`). Chips/preview row, per-attachment remove; pasted images thumbnail via `URL.createObjectURL`. `send()`: multipart `files[]` when attachments, else JSON. Client cap mirrors 64 MiB + per-file.
- **Streaming intake `[r2.16]`**: stream multipart parts directly into the durable store under the **durable-store lock** (NOT `sr.mu`), enforcing per-file + aggregate caps while writing — do NOT `io.ReadAll` every file into memory or build `[]attachment{data}` for durable paths.
- Render uses the per-turn model (§4). Storage at the shared intake seam (`enqueueToSession`/`buildTurnPrompt`) → tg/mx/vk photos also durable.

## 10. Commit order (single PR)

`storage → durable queue (incl. inbound log, recovery, turn_seq+marker) → inbound-UI (incl. per-turn render) → sealed-ref + outbound-to-UI`. Queue before inbound-UI (the §4 client model depends on it).

## 11. Performance (accepted v1 limit) `[fix17]`

- Reload reads the full backend transcript + inbound log (`handleTranscript` already reads the whole JSONL — pre-existing). Accepted for v1; the inbound log is append-only with `turn_seq`, so a `turn_seq→offset` index / bounded reverse reads are straightforward later. Add a large-synthetic-log test so behaviour is measured.

## Non-goals (v1)

- Outbound to messengers (later PR). `klax send` CLI. Content-addressed dedup. A FULL message-store (assistant/tool content stays in backend JSONL — single source of truth; klax owns only the inbound half, additive).
