// events.js — the live channel: changesLoop long-polls /api/changes with the one cursor `after` and
// hands the ordered ring events to the host. A cursor from another server process or behind the
// retained ring gets {resync:true}; the host then reloads from a snapshot.

import { api } from "./base.js";

const POLL_ABORT_MS = 30000; // > server hold (~25s); bounds a wedged request

// cursorEpoch / cursorSeq split an "<epoch>.<seq>" cursor. The epoch is a nanosecond timestamp,
// beyond exact Number range, so it stays a string.
export function cursorEpoch(c){ return String(c || "").split(".")[0]; }
export function cursorSeq(c){ return Number(String(c || "").split(".")[1]) || 0; }

// changesLoop drives the host:
//   after()/generation()           the cursor and the resync generation it belongs to
//   apply(events, at)              apply a response's events in order, then advance after to at
//   resync()                       reload from a snapshot (resolves when done)
//   onAuthFail, onHealth(ok, fails)
export async function changesLoop(host){
  let backoff = 0, fails = 0;
  const health = ok => { fails = ok ? 0 : fails + 1; if(host.onHealth) host.onHealth(ok, fails); };
  for(;;){
    // The abort timer bounds the WHOLE round-trip, body read included — a response whose headers
    // arrive but whose body then stalls (a half-open socket during a daemon restart) would otherwise
    // hang `await r.json()` and wedge the one live loop.
    const ac = new AbortController();
    const t = setTimeout(() => ac.abort(), POLL_ABORT_MS);
    const gen = host.generation(), after = host.after();
    try {
      const r = await api("/api/changes", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ after }), signal: ac.signal });
      if(r.status === 401){ if(host.onAuthFail) host.onAuthFail(); return; }
      if(!r.ok){ health(false); await sleep(backoff = nextBackoff(backoff)); continue; }
      const data = await r.json();
      backoff = 0;
      health(true);
      if(gen !== host.generation()) continue;
      if(data.resync || cursorEpoch(data.at) !== cursorEpoch(after)){ await host.resync(); continue; }
      host.apply(data.events || [], data.at);
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
