// events.js — the live channel: tailLoop long-polls /api/tail with per-session
// (turn,block,state,trail[,head]) cursors and MERGES the returned durable read-model rows
// (model.replaceTail), so live delivery and reload converge on ONE path. No epoch/ring/reload for
// content — content is recovered from the durable log; only transient notices ride a small retained buffer.

import { api } from "./base.js";

const POLL_ABORT_MS = 30000; // > server hold (~25s); bounds a wedged request

// tailLoop is the durable-tail live channel: it POSTs the client's
// per-session (turn,block,state,trail[,head]) cursors to /api/tail, MERGES the returned read-model
// rows into the model (model.replaceTail — the same rows a reload uses, so live and reload can't
// disagree), advances each cursor, applies the session strip + notices, and detects a restart via
// `started`. `host`:
//   cursors()/setTailCursor(id,c)   the per-session durable content cursors
//   noticeCursor()/setNoticeCursor(c)  the transient-notice ring cursor
//   model, ctx{onSessions,onNotice}, onAffected(set), onRestart, onAuthFail, onHealth(ok,fails)
export async function tailLoop(host){
  let backoff = 0, fails = 0, lastStarted = null;
  const health = ok => { fails = ok ? 0 : fails + 1; if(host.onHealth) host.onHealth(ok, fails); };
  for(;;){
    // The abort timer bounds the WHOLE round-trip, body read included — it is cleared in `finally`,
    // not right after the fetch. A response whose headers arrive but whose body then stalls (a
    // half-open socket during a daemon restart) would otherwise hang `await r.json()` with no
    // timeout and wedge the one live loop indefinitely, freezing timeline+badges+title until reload.
    const ac = new AbortController();
    const t = setTimeout(() => ac.abort(), POLL_ABORT_MS);
    try {
      const body = JSON.stringify({ cursors: host.cursors(), notice: host.noticeCursor(), sess_rev: host.sessRev() });
      const r = await api("/api/tail", { method: "POST", headers: { "Content-Type": "application/json" }, body, signal: ac.signal });
      if(r.status === 401){ if(host.onAuthFail) host.onAuthFail(); return; }
      if(!r.ok){ health(false); await sleep(backoff = nextBackoff(backoff)); continue; }
      const data = await r.json();
      backoff = 0;
      health(true);
      if(data.started !== undefined){
        if(lastStarted !== null && data.started !== lastStarted && host.onRestart) host.onRestart();
        lastStarted = data.started;
      }
      if(data.sessions && host.ctx.onSessions) host.ctx.onSessions(data.sessions);
      if(data.sess_rev !== undefined && host.setSessRev) host.setSessRev(data.sess_rev);
      if(data.notice !== undefined && host.setNoticeCursor) host.setNoticeCursor(data.notice);
      for(const n of (data.notices || [])){ if(host.ctx.onNotice) host.ctx.onNotice(n); }
      const affected = new Set();
      const tails = data.tails || {};
      for(const created in tails){
        const id = Number(created);
        try { host.model.replaceTail(id, tails[created].rows); host.setTailCursor(id, tails[created].cursor); affected.add(id); }
        catch(e){ console.error("klax replaceTail", created, e); }
      }
      if(affected.size) host.onAffected(affected);
    } catch(e){
      health(false);
      await sleep(backoff = nextBackoff(backoff));
    } finally {
      clearTimeout(t);
    }
  }
}

function nextBackoff(b){ return b ? Math.min(b * 2, 5000) : 500; }
function sleep(ms){ return new Promise(res => setTimeout(res, ms + Math.random() * 250)); }
