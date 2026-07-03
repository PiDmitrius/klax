// render.js — turns the read model into DOM. renderModel() is PURE (no DOM, no markdown):
// it computes ordered render items from turn.state alone — no array-index surgery, no
// runningTurn/queuedTurns guessing. A user turn is ONE {kind:"turn"} unit (its user bubble
// + grouped answer blocks + state indicator render inside one <div class="turn" data-seq>);
// standalone notices and the unread divider are their own items. paint() is the thin DOM
// layer (exercised end-to-end by the Step-4 browser harness). Markdown + capability image
// refs come from markdown.js.

import { mdSafe, esc, fmtTime, fmtDate } from "./markdown.js";

function blockCls(role){ return role === "tool" ? "tool" : role === "error" ? "error" : role === "system" ? "system" : "assistant"; }
function contextText(used, window){
  if(!used) return "";
  const uk = Math.floor(used / 1000) + "k";
  // Draw the line as soon as `used` tokens are known. A brand-new session's FIRST turn only
  // learns its window in the end-of-turn result, but Claude streams used tokens long before
  // that — so show the count now and fold in the % once the window arrives. No believed
  // window is invented: only real numbers, upgraded in place.
  if(!window) return "📊 Контекст: " + uk;
  return "📊 Контекст: " + Math.floor(used * 100 / window) + "% (" + uk + "/" + Math.floor(window / 1000) + "k)";
}

// renderModel computes the ordered render items for one session. PURE and unit-testable.
// The unread divider stays turn-aware: user bubbles are the human's own messages and do
// not count as unread; unread answer blocks land inside the turn at the exact block
// boundary, even when they have the same role as the preceding already-read block.
export function renderModel(turns, unreadAfter){
  const items = [];
  let queuePos = 0, divided = false;
  const placeDivider = () => { if(!divided && unreadAfter !== undefined){ items.push({ kind: "divider" }); divided = true; } };
  const isUnread = es => es !== undefined && unreadAfter !== undefined && es > unreadAfter;
  const blockUnread = b => b && isUnread(b.eventSeq);
  for(const t of (turns || [])){
    if(t.role !== "user"){
      if(isUnread(t.eventSeq)) placeDivider();
      if(t.kind === "compact") items.push({ kind: "bubble", cls: "system", text: "🗜 контекст свёрнут", md: false, time: t.time, maxEventSeq: t.eventSeq });
      else if(t.role === "notice") items.push({ kind: "bubble", cls: "notice", text: t.text || "", md: false, time: t.time, maxEventSeq: t.eventSeq });
      else items.push({ kind: "bubble", cls: (t.kind === "error" || t.role === "error") ? "error" : t.role === "system" ? "system" : "assistant", text: t.text || "", md: true, time: t.time, maxEventSeq: t.eventSeq });
      continue;
    }
    const groups = [];
    let i = 0;
    let lastGroupTime = t.time;
    while(i < (t.blocks || []).length){
      if(blockUnread(t.blocks[i]) && !divided && unreadAfter !== undefined){
        groups.push({ divider: true });
        divided = true;
        continue;
      }
      const role = t.blocks[i].role, blocks = [];
      while(i < t.blocks.length && t.blocks[i].role === role){
        if(blockUnread(t.blocks[i]) && !divided && unreadAfter !== undefined && blocks.length > 0) break;
        blocks.push(t.blocks[i]); i++;
      }
      const seqs = blocks.map(b => b.eventSeq || 0);
      const last = blocks.length ? blocks[blocks.length - 1] : {};
      const group = {
        cls: blockCls(role), blocks, tool: role === "tool", time: last.time,
        maxEventSeq: Math.max(0, ...seqs) || undefined,
      };
      groups.push(group);
      if(group.time) lastGroupTime = group.time;
    }
    if(t.state === "enq") queuePos++;
    // Context is ONE left tool-line — the turn's "cut line", always the LAST element of the
    // turn: BELOW the working dots, not above them. While the turn runs the order is
    // [answer][dots][context] — new content is born at the dots, the context line stays at
    // the bottom; when the turn settles the dots vanish and the context slides up to close
    // the gap, still the bottom line. Live % (session hint as fallback) while running; the
    // settled line uses turn-local context only, so a stale hint never becomes a fake final.
    // It is NOT a group — buildItem renders it after the dots indicator.
    const finalCtx = contextText(t.ctx_used, t.ctx_window);
    const ctxLine = (t.state === "done" || t.state === "err") ? finalCtx
      : t.state === "run" ? (finalCtx || contextText(t.ctx_hint_used, t.ctx_hint_window))
      : "";
    const note = t.state === "enq" ? "в очереди · " + queuePos : undefined;
    items.push({ kind: "turn", seq: t.seq, text: t.text || "", time: t.time, groups, state: t.state, note, ctxLine, ctxTime: lastGroupTime });
  }
  return items;
}

// --- DOM layer (thin) ---
const DOTS = '<span class="dots"><span></span><span></span><span></span></span>';

function divider(){
  const d = document.createElement("div");
  d.className = "readline"; d.innerHTML = "<span>непрочитанные сообщения</span>";
  return d;
}

function timeMeta(time){
  if(!time) return "";
  const u = typeof time === "number" ? time : Date.parse(time);
  return u ? esc(fmtDate(u))+' '+esc(fmtTime(u)) : "";
}
function metaHTML(time){
  const tm = timeMeta(time);
  return tm ? '<span class="meta">'+tm+'</span>' : "";
}
// bubble carries the PRIMARY text on the node (`_raw`): the whole-message copy button
// puts the model text on the clipboard, never the rendered HTML (app.js delegate).
function bubble(cls, html, time, maxEventSeq, raw){
  const meta = metaHTML(time);
  const d = document.createElement("div");
  d.className = "msg " + cls + (meta ? " hasmeta" : "");
  if(maxEventSeq) d.dataset.maxEventSeq = String(maxEventSeq);
  const copy = raw ? '<button class="mcopy" title="Копировать сообщение">⧉</button>' : "";
  d.innerHTML = copy + '<div class="body">'+html+'</div>' + meta;
  if(raw) d._raw = raw;
  return d;
}
// indicator is the per-turn tail dots: null for a settled turn (done/err — err shows its
// error block); animated + ✕ for run; dim 'в очереди · N' for enq.
function indicator(state, note, onAbort){
  if(state === "done" || state === "err" || state === undefined) return null;
  const animated = state === "run";
  const abortable = state === "run" || state === "enq";
  const d = document.createElement("div");
  d.className = "msg assistant typing" + (animated ? "" : " queued");
  // The only note left is the enq queue position ('в очереди · N'). The context line is a
  // separate tool bubble rendered below the dots (see buildItem), so the running indicator
  // is just the animated dots + the ✕ abort button.
  const queueNote = note ? '<span class="qnote">'+esc(note)+'</span>' : "";
  d.innerHTML = DOTS + queueNote + (abortable ? '<button class="stop" title="Прервать">✕</button>' : "");
  if(abortable && onAbort){ const b = d.querySelector(".stop"); if(b) b.addEventListener("click", onAbort); }
  return d;
}

function reusableImages(col){
  const bySrc = new Map();
  col.querySelectorAll("img.att[src]").forEach(img => {
    const src = img.getAttribute("src");
    if(!src) return;
    const list = bySrc.get(src) || [];
    list.push(img);
    bySrc.set(src, list);
  });
  return bySrc;
}

function reuseImages(root, bySrc){
  root.querySelectorAll("img.att[src]").forEach(img => {
    const src = img.getAttribute("src");
    const list = src && bySrc.get(src);
    const old = list && list.shift();
    if(old) img.replaceWith(old);
  });
}

function renderKey(it, index){
  if(it.kind === "turn") return "turn:" + it.seq;
  // Prefer the event seq over the list index: it keeps the key stable when items above
  // (the unread divider) come and go, which node reuse and FLIP shifts both rely on.
  if(it.kind === "bubble") return "bubble:" + (it.maxEventSeq !== undefined ? "e" + it.maxEventSeq : index) + ":" + it.cls;
  return "";
}

function renderSig(it){
  if(it.kind === "turn"){
    return JSON.stringify({
      text: it.text, time: it.time, state: it.state, note: it.note, ctxLine: it.ctxLine,
      groups: it.groups.map(g => ({
        divider: g.divider,
        cls: g.cls, tool: g.tool, time: g.time, maxEventSeq: g.maxEventSeq,
        blocks: (g.blocks || []).map(b => ({ id: b.id, role: b.role, text: b.text, kind: b.kind, time: b.time })),
      })),
    });
  }
  if(it.kind === "bubble") return JSON.stringify({ cls: it.cls, text: it.text, md: it.md, time: it.time, maxEventSeq: it.maxEventSeq });
  return "";
}

function reusableNodes(col){
  const byKey = new Map();
  Array.from(col.children).forEach(node => { if(node.dataset && node.dataset.renderKey) byKey.set(node.dataset.renderKey, node); });
  return byKey;
}

function stamp(node, key, sig){
  if(key){
    node.dataset.renderKey = key;
    node.dataset.renderSig = sig;
  }
  return node;
}

// withFlip tags a turn child as an independently animatable FLIP unit. A group bubble is
// keyed by its FIRST block's id: when reading merges the bubbles a divider used to split,
// the merged bubble inherits the leading part's identity and stays put — only the content
// below slides up into the collapsed gap.
function withFlip(el, key){ el.dataset.flip = key; return el; }

function buildItem(it, onAbort){
  if(it.kind === "divider") return divider();
  if(it.kind === "bubble") return bubble(it.cls, it.md ? mdSafe(it.text) : esc(it.text), it.time, it.maxEventSeq, it.text);
  const turn = document.createElement("div");
  turn.className = "turn"; turn.dataset.seq = it.seq;
  turn.appendChild(withFlip(bubble("user", mdSafe(it.text), it.time, undefined, it.text), "u"));
  let gi = 0;
  for(const g of it.groups){
    if(g.divider){ turn.appendChild(divider()); continue; }
    const html = g.blocks.map(b => g.tool ? esc(b.text || "") : mdSafe(b.text || "")).join(g.tool ? "<br>" : "");
    const raw = g.blocks.map(b => b.text || "").join(g.tool ? "\n" : "\n\n");
    const fk = "g:" + ((g.blocks[0] && g.blocks[0].id) || (g.cls + ":" + gi));
    turn.appendChild(withFlip(bubble(g.cls, html, g.time, g.maxEventSeq, raw), fk));
    gi++;
  }
  const ind = indicator(it.state, it.note, onAbort);
  if(ind) turn.appendChild(withFlip(ind, "dots"));
  // The context "cut line" is the turn's final element — below the dots while running, and
  // the last line once the dots are gone (it slides up to close the gap).
  if(it.ctxLine){
    turn.appendChild(withFlip(bubble("tool", esc(it.ctxLine), it.ctxTime, undefined, it.ctxLine), "g:ctx:" + it.seq));
  }
  return turn;
}

export function paint(col, items, onAbort){
  const nodes = reusableNodes(col);
  const frag = document.createDocumentFragment();
  items.forEach((it, index) => {
    const key = renderKey(it, index);
    const sig = renderSig(it);
    const old = key && nodes.get(key);
    if(old && old.dataset.renderSig === sig){
      nodes.delete(key); // a duplicate key later must build fresh, not steal this node
      frag.appendChild(old);
      return;
    }
    frag.appendChild(stamp(buildItem(it, onAbort), key, sig));
  });
  const images = reusableImages(col);
  reuseImages(frag, images);
  col.replaceChildren(frag);
}

export function renderSession(col, turns, unreadAfter, onAbort){
  paint(col, renderModel(turns, unreadAfter), onAbort);
}

// --- smooth live updates (FLIP) ---
// beginShift snapshots visual positions before a LIVE re-render; playShift then slides
// every surviving unit from its old position to the new one, fades a ghost of a vanished
// unread divider in place while the messages below close the gap, and gives genuinely new
// nodes a short entry animation. A FLIP *unit* is what actually moves: a standalone keyed
// bubble, or a CHILD of a .turn (user bubble / answer group / indicator, keyed
// turnKey|data-flip) — the .turn container itself is never transformed, so parent and
// child shifts cannot compound, and an in-turn divider collapse animates block-level.
// Transforms only — layout and all scroll maths stay exact. Reduced motion disables it.
const SHIFT_MS = 180, SHIFT_CAP = 800;

function reducedMotion(){
  return typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
}

export function beginShift(col){
  if(reducedMotion()) return null;
  const units = new Map(), keys = new Set();
  Array.from(col.children).forEach(node => {
    const key = node.dataset && node.dataset.renderKey;
    if(!key) return;
    keys.add(key);
    if(node.classList.contains("turn")){
      Array.from(node.children).forEach(ch => {
        const fk = ch.dataset && ch.dataset.flip;
        if(fk) units.set(key + "|" + fk, ch.getBoundingClientRect().top); // visual pos, mid-animation included
      });
    } else {
      units.set(key, node.getBoundingClientRect().top);
    }
  });
  col.querySelectorAll(".enter").forEach(n => n.classList.remove("enter"));
  const dv = col.querySelector(".readline");
  return { units, keys, divider: dv ? dv.getBoundingClientRect() : null, hadAny: col.children.length > 0 };
}

export function playShift(col, snap){
  if(!snap) return;
  const nodes = [], freshTurns = [];
  Array.from(col.children).forEach(node => {
    const key = node.dataset && node.dataset.renderKey;
    if(!key) return;
    if(node.classList.contains("turn")){
      if(!snap.keys.has(key)){ freshTurns.push(node); return; } // brand-new turn enters as one
      Array.from(node.children).forEach(ch => {
        const fk = ch.dataset && ch.dataset.flip;
        if(fk) nodes.push([ch, key + "|" + fk]);
      });
    } else if(!snap.keys.has(key)){
      freshTurns.push(node);
    } else {
      nodes.push([node, key]);
    }
  });
  nodes.forEach(([el]) => { el.style.transition = "none"; el.style.transform = ""; }); // neutralize leftovers
  const vh = typeof innerHeight === "number" ? innerHeight : 800;
  const inView = r => r.top < vh * 1.5 && r.bottom > -100;
  const shifts = [], fresh = [];
  nodes.forEach(([el, uk]) => { // one layout pass: everything at its final position
    const r = el.getBoundingClientRect();
    if(!snap.units.has(uk)){
      if(snap.hadAny && inView(r)) fresh.push(el);
      return;
    }
    const d = snap.units.get(uk) - r.top;
    if(Math.abs(d) >= 2 && Math.abs(d) <= SHIFT_CAP && r.top > -vh && r.top < vh * 2) shifts.push([el, d]);
  });
  if(snap.hadAny) freshTurns.forEach(el => { if(inView(el.getBoundingClientRect())) fresh.push(el); });
  shifts.forEach(([el, d]) => { el.style.transform = "translateY(" + d + "px)"; });
  fresh.forEach(el => {
    el.classList.add("enter");
    el.addEventListener("animationend", () => el.classList.remove("enter"), { once: true });
  });
  if(shifts.length){
    void col.offsetHeight; // commit the start positions before transitioning
    shifts.forEach(([el]) => {
      el.style.transition = "transform " + SHIFT_MS + "ms ease-out";
      el.style.transform = "";
      el.addEventListener("transitionend", () => { el.style.transition = ""; }, { once: true });
    });
  }
  if(snap.divider && !col.querySelector(".readline")) fadeDividerGhost(snap.divider);
}

// clearShiftGhosts removes any fading divider ghost — structural renders (tab switch,
// transcript reload, pagination) must not inherit a ghost from the previous view.
export function clearShiftGhosts(){
  const wrap = document.getElementById("logwrap");
  if(wrap) wrap.querySelectorAll(".readline.ghost").forEach(n => n.remove());
}

// The removed "непрочитанные сообщения" line fades out exactly where it stood (an
// absolutely positioned ghost in #logwrap) while the FLIP above collapses the gap.
function fadeDividerGhost(rect){
  const wrap = document.getElementById("logwrap");
  if(!wrap) return;
  const w = wrap.getBoundingClientRect();
  if(rect.bottom < w.top - 40 || rect.top > w.bottom + 40) return; // was offscreen anyway
  const old = wrap.querySelector(".readline.ghost");
  if(old) old.remove();
  const g = divider();
  g.className = "readline ghost";
  g.style.top = (rect.top - w.top) + "px";
  g.style.left = (rect.left - w.left) + "px";
  g.style.width = rect.width + "px";
  wrap.appendChild(g);
  const drop = () => g.remove();
  g.addEventListener("animationend", drop, { once: true });
  setTimeout(drop, 500);
}
