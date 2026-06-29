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

  // appendStandalone adds a non-turn row (a compact/system notice between turns).
  appendStandalone(created, row){
    const arr = this.byCreated[created] = this.byCreated[created] || [];
    arr.push({ role: row.role || "system", kind: row.kind, text: row.text, eventSeq: row.eventSeq });
  }

}

function normTurn(r){
  return {
    seq: r.seq, role: r.role, text: r.text, time: r.time, state: r.state, kind: r.kind,
    eventSeq: r.eventSeq, blocks: (r.blocks || []).map(b => ({ ...b })),
  };
}
