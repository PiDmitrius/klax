// events.js — the live channel: changesLoop long-polls /api/changes with the one cursor `after` and
// hands the ordered ring events to the host. A cursor from another server process or behind the
// retained ring gets {resync:true}; the host then reloads from a snapshot. After a failure the next
// request asks for an immediate answer, so recovery is reported within one round trip instead of
// after a full idle hold; wakeChanges cuts the retry pause short when the page comes back.

import { api, retryDelay } from "./base.js";

// cursorEpoch / cursorSeq split an "<epoch>.<seq>" cursor; the epoch is an opaque process id.
export function cursorEpoch(c){ return String(c || "").split(".")[0]; }
export function cursorSeq(c){ return Number(String(c || "").split(".")[1]) || 0; }

// changesLoop drives the host:
//   after()                        the cursor
//   apply(events, at)              apply a response's events in order, then advance after to at
//   resync()                       reload from a snapshot (resolves when done)
//   onAuthFail, onHealth(ok, fails)
export async function changesLoop(host){
  let attempt = 0, fails = 0, nowait = false;
  const health = ok => { fails = ok ? 0 : fails + 1; if(host.onHealth) host.onHealth(ok, fails); };
  for(;;){
    const after = host.after();
    try {
      const r = await api("/api/changes", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ after, nowait }) }, true);
      if(r.status === 401){ if(host.onAuthFail) host.onAuthFail(); return; }
      if(!r.ok) throw new Error("changes HTTP " + r.status);
      const data = await r.json();
      if(data.resync || cursorEpoch(data.at) !== cursorEpoch(after)) await host.resync();
      else host.apply(data.events || [], data.at);
      attempt = 0;
      nowait = false;
      health(true);
    } catch(e){
      nowait = true;
      health(false);
      await pause(retryDelay(attempt++));
    }
  }
}

let wake = null;
export function wakeChanges(){ if(wake) wake(); }
function pause(ms){
  return new Promise(res => {
    const done = () => { wake = null; res(); };
    const timer = setTimeout(done, ms);
    wake = () => { clearTimeout(timer); done(); };
  });
}
