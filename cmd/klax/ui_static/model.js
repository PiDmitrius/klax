// model.js — the per-session read model, the single client-side source of truth. A session holds
// the server's turn groups (a user row with its blocks, plus the standalone rows that follow it)
// ordered by `ord`, and the start of the range it holds. Windows and pages replace a range; group
// events patch one group. turns() is the flat row list render works on. Pure data; no DOM, no fetch.

// ordLess orders two group positions [event|null, seq]; a null event sorts after every event.
export function ordLess(a, b){
  if((a[0] === null) !== (b[0] === null)) return b[0] === null;
  if(a[0] !== null && a[0] !== b[0]) return a[0] < b[0];
  return a[1] < b[1];
}

export function ordParam(o){ return (o[0] === null ? "null" : o[0]) + "," + o[1]; }

// applyMerge applies a JSON Merge Patch to an object: a key set to null is dropped.
export function applyMerge(obj, patch){
  const out = { ...(obj || {}) };
  for(const k in patch){ if(patch[k] === null) delete out[k]; else out[k] = patch[k]; }
  return out;
}

// applyArray applies an array delta: patches by index, then the elements appended from `start`,
// then a shorter length. Every step is positional, so a repeated delta changes nothing. Null when
// the array is too short for the delta: the client lost track of it.
function applyArray(arr, d){
  if(!d) return arr;
  if(arr.length < (d.start || 0) || Object.keys(d.set || {}).some(i => +i >= arr.length)) return null;
  const out = arr.slice();
  for(const i in (d.set || {})) out[i] = applyMerge(out[i], d.set[i]);
  if(d.append){ out.length = d.start || 0; for(const e of d.append) out.push(e); }
  if(d.length !== undefined) out.length = d.length;
  return out;
}

export class TurnModel {
  constructor(){ this.byCreated = {}; }

  has(created){ return this.byCreated[created] !== undefined; }
  drop(created){ delete this.byCreated[created]; }
  rangeStart(created){ const s = this.byCreated[created]; return s ? s.from : null; }

  turns(created){
    const s = this.byCreated[created];
    if(!s) return [];
    if(!s.rows){
      s.rows = [];
      for(const g of s.groups){
        if(g.head) s.rows.push({ ...g.head, blocks: g.blocks });
        g.rows.forEach((r, i) => s.rows.push({ ...r, key: g.key + ":" + i }));
      }
    }
    return s.rows;
  }

  // loadWindow replaces a session with the newest window.
  loadWindow(created, w){
    this.byCreated[created] = { groups: (w.groups || []).map(normGroup), from: w.from, rows: null };
  }

  // loadPage merges an older page: it replaces every held group inside [from, to) and every held
  // copy of a key the page carries (a group that moved into the page's range).
  loadPage(created, w){
    const s = this.byCreated[created];
    if(!s) return;
    const inside = o => !ordLess(o, w.from) && (!w.to || ordLess(o, w.to));
    const keys = new Set((w.groups || []).map(g => g.key));
    s.groups = s.groups.filter(g => !inside(g.ord) && !keys.has(g.key)).concat((w.groups || []).map(normGroup));
    s.groups.sort((a, b) => ordLess(a.ord, b.ord) ? -1 : 1);
    if(ordLess(w.from, s.from)) s.from = w.from;
    s.rows = null;
  }

  // applyGroup applies one group event — a full create, or patches of the head, blocks and rows;
  // returns false when the session lost track of that group (a patch for a group it should hold but
  // does not) and needs a fresh window.
  applyGroup(created, d){
    const s = this.byCreated[created];
    if(!s) return true;
    const i = s.groups.findIndex(g => g.key === d.key);
    if(i < 0){
      if(ordLess(d.ord, s.from)) return true;
      if(!d.create) return false;
      s.groups.push(normGroup({ key: d.key, ord: d.ord, ...d.create }));
    } else {
      const g = s.groups[i];
      if(ordLess(d.ord, s.from)) return false; // a held group moved below the range: the history was renumbered
      if(d.create) s.groups[i] = normGroup({ key: d.key, ord: d.ord, ...d.create });
      else {
        const blocks = applyArray(g.blocks, d.blocks), rows = applyArray(g.rows, d.rows);
        if(!blocks || !rows) return false;
        g.ord = d.ord;
        if(d.head) g.head = applyMerge(g.head, d.head);
        g.blocks = blocks;
        g.rows = rows;
      }
    }
    s.groups.sort((a, b) => ordLess(a.ord, b.ord) ? -1 : 1);
    s.rows = null;
    return true;
  }

  applyRemoved(created, key){
    const s = this.byCreated[created];
    if(!s) return;
    const i = s.groups.findIndex(g => g.key === key);
    if(i >= 0){ s.groups.splice(i, 1); s.rows = null; }
  }

  // evictTop drops whole groups covered by the oldest n rows (windowing — early history unloaded to
  // keep a long session responsive) and raises the range start. Returns the rows actually removed.
  evictTop(created, n){
    const s = this.byCreated[created];
    if(!s || n <= 0) return 0;
    let groups = 0, rows = 0;
    while(groups < s.groups.length - 1){
      const g = s.groups[groups], size = (g.head ? 1 : 0) + g.rows.length;
      if(rows + size > n) break;
      rows += size; groups++;
    }
    // The range starts at a transcript position: not at a queued turn, which sorts last or shares
    // the event of the transcript turn after it.
    const queued = i => s.groups[i].ord[0] === null || s.groups[i].ord[0] === s.groups[i + 1]?.ord[0];
    while(groups > 0 && queued(groups)) groups--;
    if(!groups) return 0;
    s.groups = s.groups.slice(groups);
    s.from = s.groups[0].ord;
    s.rows = null;
    return rows;
  }
}

function normGroup(g){
  return { key: g.key, ord: g.ord, head: g.head || null, blocks: g.blocks || [], rows: g.rows || [] };
}
