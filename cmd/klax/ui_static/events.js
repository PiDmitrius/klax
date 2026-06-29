// events.js — the live channel. applyEvent patches the model from ONE server event
// (pure-ish: model ops + ctx callbacks, unit-testable). pollLoop drives it: long-poll
// /api/poll, apply the batch in order (deduped by event seq + block id), and ask the host
// to re-render affected sessions or reload from the transcript when the cursor is
// uncoverable. No runningTurn/queuedTurns/readMark — a turn's truth is the server's state.

import { api } from "./base.js";

// applyEvent applies one event to the model; returns the affected session id (to
// re-render) or null. ctx: { onSessions(list), onNotice(text) }.
export function applyEvent(model, ev, ctx){
  ctx = ctx || {};
  const s = ev.session;
  switch(ev.type){
    case "user":
      model.upsertUser(s, { seq: ev.turn_seq, text: ev.text, time: ev.time, eventSeq: ev.seq }, ev.state || "enq");
      return s;
    case "turn_start":
      model.setState(s, ev.turn_seq, "run");
      return s;
    case "progress":
      if(ev.block){ ev.block.eventSeq = ev.seq; model.appendBlock(s, ev.turn_seq, ev.block); }
      return s;
    case "final":
      if(ev.block){ ev.block.eventSeq = ev.seq; model.appendBlock(s, ev.turn_seq, ev.block); }
      model.setState(s, ev.turn_seq, "done");
      return s;
    case "error":
      if(ev.block){ ev.block.eventSeq = ev.seq; model.appendBlock(s, ev.turn_seq, ev.block); }
      model.setState(s, ev.turn_seq, "err");
      return s;
    case "compact":
      model.appendStandalone(s, { role: "system", kind: "compact", eventSeq: ev.seq });
      return s;
    case "sessions":
      if(ctx.onSessions) ctx.onSessions(ev.sessions);
      return null;
    case "notice":
      if(ctx.onNotice) ctx.onNotice(ev.text);
      return null;
  }
  return null;
}

// --- poll loop (browser) ---
const POLL_ABORT_MS = 30000; // > server hold (~25s); bounds a wedged request

// pollLoop long-polls forever, applying each batch. `host` provides:
//   cursor() / setCursor(c)      the poll cursor "<epoch>-<seq>"
//   lastSeq() / setLastSeq(n)    highest applied event seq (dedup)
//   model, ctx                   the TurnModel + applyEvent ctx
//   onAffected(Set<created>)     re-render these sessions
//   onReload()                   the cursor is uncoverable — reload from /api/sessions + transcript
export async function pollLoop(host){
  let backoff = 0, lastEpoch = null;
  for(;;){
    try {
      const ac = new AbortController();
      const t = setTimeout(() => ac.abort(), POLL_ABORT_MS);
      const cursor = host.cursor();
      const r = await api("/api/poll" + (cursor ? "?cursor=" + encodeURIComponent(cursor) : ""), { signal: ac.signal });
      clearTimeout(t);
      if(r.status === 401){ if(host.onAuthFail) host.onAuthFail(); return; } // expired/invalid token → back to the gate
      if(!r.ok){ await sleep(backoff = nextBackoff(backoff)); continue; }
      const data = await r.json();
      backoff = 0;
      // A changed epoch means the daemon restarted (vs a same-epoch ring-overflow reload).
      if(data.epoch !== undefined){
        if(lastEpoch !== null && data.epoch !== lastEpoch && host.onRestart) host.onRestart();
        lastEpoch = data.epoch;
      }
      // Uncoverable cursor → a single-owner, awaited reload: onReload() reconciles the
      // model from /api/transcript and owns setting cursor = lastSeq = its WATERMARK (§A3).
      // We do NOT advance to the poll's reload cursor here (that's the hub head, not the
      // transcript watermark) and we await so live polling can't race the reconcile.
      if(data.reload){ await host.onReload(); continue; }
      const affected = new Set();
      for(const raw of (data.events || [])){
        let ev;
        try { ev = typeof raw === "string" ? JSON.parse(raw) : raw; } catch(e){ continue; }
        if(ev.seq !== undefined && ev.seq <= host.lastSeq()) continue; // already applied
        try { const s = applyEvent(host.model, ev, host.ctx); if(s !== null && s !== undefined) affected.add(s); }
        catch(e){ console.error("klax applyEvent", ev && ev.type, e); }
        if(ev.seq !== undefined) host.setLastSeq(ev.seq); // advance past it either way — never re-loop a bad event
      }
      // Advance the cursor ONLY after the whole batch applied, so a mid-batch throw can't
      // skip unapplied events (the next poll re-fetches from the un-advanced cursor).
      if(data.cursor) host.setCursor(data.cursor);
      if(affected.size) host.onAffected(affected);
    } catch(e){
      await sleep(backoff = nextBackoff(backoff));
    }
  }
}

function nextBackoff(b){ return b ? Math.min(b * 2, 5000) : 500; }
function sleep(ms){ return new Promise(res => setTimeout(res, ms + Math.random() * 250)); }
