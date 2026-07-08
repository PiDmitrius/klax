// model.js — the per-session read model: one ordered array of turns per session, the
// single client-side source of truth. Pure data + idempotent/monotonic patch ops; no
// DOM, no fetch. The server (transcript ⋈ queue.jsonl) is authoritative: reconcile()
// replaces a session from a reload, and the live events patch it (upsertUser / setState /
// appendBlock). This replaces the old runningTurn/doneTurns/queuedTurns/
// tmpTurn machine — a turn's state is just `turn.state`.

// Monotonic order: enq → run → done|err. A terminal state (rank 3) never regresses, so a
// late progress/turn_start after a final can't un-finish a turn.
const RANK = { enq: 1, run: 2, done: 3, err: 3 };

export function isPending(state){ return state === "enq" || state === "run"; }

function advance(cur, next){
  if(cur === undefined) return next || cur;
  if(!next) return cur;
  if((RANK[cur] || 0) >= 3) return cur;                          // terminal wins
  return (RANK[next] || 0) >= (RANK[cur] || 0) ? next : cur;
}

export class TurnModel {
  constructor(){ this.byCreated = {}; }

  turns(created){ return this.byCreated[created] || []; }
  has(created){ return this.byCreated[created] !== undefined; }
  drop(created){ delete this.byCreated[created]; }

  // reconcile replaces a session's turns from a reload's read-model rows (authoritative).
  reconcile(created, rows){
    this.byCreated[created] = (rows || []).map(normTurn);
  }

  // prepend inserts an older pagination page at the FRONT (load-earlier history).
  prepend(created, rows){
    const arr = this.byCreated[created] = this.byCreated[created] || [];
    this.byCreated[created] = (rows || []).map(normTurn).concat(arr);
  }

  // evictTop drops the oldest n rows (windowing — early history unloaded from the timeline to keep
  // a long session responsive). Returns the count actually removed. Each removed row is one
  // transcript page-unit, so the caller advances its loadOlder offset by the return value; the
  // caller also guarantees it never evicts an unread row (see app.js capWindow).
  evictTop(created, n){
    const arr = this.byCreated[created];
    if(!arr || n <= 0) return 0;
    n = Math.min(n, arr.length);
    if(n <= 0) return 0;
    this.byCreated[created] = arr.slice(n);
    return n;
  }

  // replaceTail merges a live tail from /api/tail — the durable read model from the boundary turn
  // onward. It drops the existing tail from that turn (with any trailing standalone rows) and
  // appends the fresh rows, so a grown last turn and brand-new turns re-sync in ONE step — the SAME
  // buildReadModel rows a reload uses, so live and reload can't disagree. Empty rows are a no-op.
  // [DURABLE_CURSOR_PLAN.md S4 — client merges rows, not events]
  replaceTail(created, rows){
    rows = (rows || []).map(normTurn);
    if(!rows.length) return;
    const arr = this.byCreated[created] || [];
    let fromSeq;
    for(const t of rows){ if(t.role === "user" && t.seq !== undefined){ fromSeq = t.seq; break; } }
    if(fromSeq === undefined){ this.byCreated[created] = rows; return; } // no user anchor → replace whole
    let cut = arr.length;
    for(let i = 0; i < arr.length; i++){
      const t = arr[i];
      if(t.role === "user" && t.seq !== undefined && t.seq >= fromSeq){ cut = i; break; }
    }
    this.byCreated[created] = arr.slice(0, cut).concat(rows);
  }

  _seq(arr, seq){ for(const t of arr){ if(t.role === "user" && t.seq === seq) return t; } }

  // upsertUser inserts or updates a user turn from a `user` event / send response.
  upsertUser(created, fields, state){
    const arr = this.byCreated[created] = this.byCreated[created] || [];
    let t = this._seq(arr, fields.seq);
    if(!t){ t = { seq: fields.seq, role: "user", blocks: [] }; arr.push(t); }
    if(fields.text !== undefined) t.text = fields.text;
    if(fields.time !== undefined) t.time = fields.time;
    if(fields.eventSeq) t.eventSeq = fields.eventSeq;
    t.state = advance(t.state, state);
    return t;
  }

  // setState advances a turn's state monotonically (turn_start / final / error).
  setState(created, seq, state){
    const t = this._seq(this.turns(created), seq);
    if(t) t.state = advance(t.state, state);
  }

  // setContext updates the turn-level usage snapshot. It is intentionally separate from
  // blocks: the working indicator and the final context line are per-turn UI, not per-bubble.
  setContext(created, seq, used, window){
    const t = this._seq(this.turns(created), seq);
    if(!t) return;
    if(used) t.ctx_used = used;
    if(window) t.ctx_window = window;
  }

  // appendBlock adds an answer block, deduped by block.id: a present id wins (a live
  // eventSeq is attached to the existing block so unread still sees it), else it appends.
  // Visual merging of consecutive same-role blocks is render's job, not the model's.
  appendBlock(created, seq, block){
    const t = this._seq(this.turns(created), seq);
    if(!t) return;
    const existing = block.id && t.blocks.find(b => b.id === block.id);
    if(existing){ if(block.eventSeq) existing.eventSeq = block.eventSeq; return; }
    t.blocks.push({ ...block });
  }

  // appendStandalone adds a non-turn, non-durable row between turns.
  appendStandalone(created, row){
    const arr = this.byCreated[created] = this.byCreated[created] || [];
    arr.push({ role: row.role || "system", kind: row.kind, text: row.text, eventSeq: row.eventSeq });
  }

}

function normTurn(r){
  return {
    seq: r.seq, role: r.role, text: r.text, time: r.time, state: r.state, kind: r.kind,
    eventSeq: r.eventSeq, ctx_used: r.ctx_used, ctx_window: r.ctx_window,
    blocks: (r.blocks || []).map(b => ({ ...b })),
  };
}
