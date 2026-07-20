// render.js — turns the read model into DOM. renderModel() is PURE (no DOM, no markdown):
// it computes ordered render items from turn.state alone — no array-index surgery, no
// runningTurn/queuedTurns guessing. A user turn is ONE {kind:"turn"} unit (its user bubble
// + grouped answer blocks + state indicator render inside one <div class="turn" data-seq>);
// standalone transcript rows and the unread divider are their own items. paint() is the thin DOM
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

// The unread axis is the DURABLE (turn_seq, block index) position, encoded as one comparable
// number so a read watermark stays a scalar (`readThrough`). A block index never approaches
// POS_MULT, so pos() is monotonic and collision-free; parse/decode bridge the "<turn>.<block>"
// wire form the server persists. Legacy markerless turns have a negative seq ⇒ their positions sort
// before any real watermark ⇒ always "read", exactly as the server's unreadAfter treats them.
export const POS_MULT = 1e6;
export function pos(turn, block){ return turn * POS_MULT + block; }
export function parsePos(s){
  const i = String(s || "").indexOf(".");
  if(i < 0) return 0;
  return pos(parseInt(String(s).slice(0, i), 10) || 0, parseInt(String(s).slice(i + 1), 10) || 0);
}
export function decodePos(p){ p = p || 0; return { turn: Math.floor(p / POS_MULT), block: p % POS_MULT }; }

// renderModel computes the ordered render items for one session. PURE and unit-testable.
// The unread divider stays turn-aware: user bubbles are the human's own messages and do not count
// as unread; standalone non-durable rows are not counted and just flow to the right
// side of the divider by document order; unread answer blocks land inside the turn at the exact
// block boundary. `watermark` is the encoded read position (pos()); undefined ⇒ no divider.
// `holdSplits` preserves group boundaries that used to be separated by the unread divider for one
// live frame after the divider disappears. That lets the line fade out before the two bubble pieces
// merge back into one.
export function renderModel(turns, watermark, holdSplits, joinHeldSplits){
  const items = [];
  let queuePos = 0, divided = false;
  // The last context we actually know — the most recent turn's own measured value (the exact
  // number already drawn on its line). A running turn with no tokens of its own yet falls back to
  // THIS, so the carried hint is the same one source, never a second parallel count that can disagree.
  let lastCtxUsed = 0, lastCtxWindow = 0;
  const has = watermark !== undefined;
  const unread = p => has && p > watermark;
  for(const t of (turns || [])){
    if(t.role !== "user"){
      // System notices belong exclusively to the notification stack, never the timeline.
      if(t.role === "notice") continue;
      // `key` is the standalone's live eventSeq (render-key stability only); it carries no data-pos
      // so it never drives read-advance, and it is not counted as unread.
      items.push({ kind: "bubble", cls: (t.kind === "error" || t.role === "error") ? "error" : t.role === "system" ? "system" : t.role === "tool" ? "tool" : "assistant", text: t.text || "", md: t.role !== "tool", time: t.time, key: t.eventSeq });
      continue;
    }
    const blocks_ = t.blocks || [];
    const groups = [];
    const held = holdSplits && holdSplits.get && holdSplits.get(t.seq);
    let i = 0;
    let lastGroupTime = t.time;
    while(i < blocks_.length){
      if(unread(pos(t.seq, i)) && !divided && has){
        groups.push({ divider: true });
        divided = true;
        continue;
      }
      const role = blocks_[i].role, blocks = [];
      const groupStart = i;
      while(i < blocks_.length && blocks_[i].role === role){
        if(held && i > groupStart && held.has(pos(t.seq, i))) break;
        if(unread(pos(t.seq, i)) && !divided && has && blocks.length > 0) break;
        blocks.push(blocks_[i]); i++;
      }
      const last = blocks.length ? blocks[blocks.length - 1] : {};
      const startPos = pos(t.seq, i - blocks.length);
      const group = {
        cls: blockCls(role), blocks, tool: role === "tool", time: last.time,
        startPos,
        maxPos: pos(t.seq, i - 1), // the last block's position — drives read-advance (data-pos)
      };
      groups.push(group);
      if(group.time) lastGroupTime = group.time;
    }
    if(joinHeldSplits && held){
      for(let j = 1; j < groups.length; j++){
        const prev = groups[j - 1], cur = groups[j];
        if(!prev || !cur || prev.divider || cur.divider) continue;
        if(!held.has(cur.startPos) || prev.cls !== cur.cls || prev.tool !== cur.tool) continue;
        prev.joinNext = true;
        cur.joinPrev = true;
      }
    }
    if(t.state === "enq") queuePos++;
    // Context is ONE left tool-line — the turn's "cut line", always the LAST element of the
    // turn: BELOW the working dots, not above them. While the turn runs the order is
    // [answer][dots][context] — new content is born at the dots, the context line stays at
    // the bottom; when the turn settles the dots vanish and the context slides up to close
    // the gap, still the bottom line. A running turn with no tokens of its own yet falls back to
    // lastCtx — the previous turn's final value, already known and already shown on its line.
    // It is NOT a group — buildItem renders it after the dots indicator.
    const finalCtx = contextText(t.ctx_used, t.ctx_window);
    const ctxLine = (t.state === "done" || t.state === "err") ? finalCtx
      : t.state === "run" ? (finalCtx || contextText(lastCtxUsed, lastCtxWindow))
      : "";
    const note = t.state === "enq" ? "в очереди · " + queuePos
      : t.state === "unknown" ? "статус неизвестен" : undefined;
    items.push({ kind: "turn", seq: t.seq, text: t.text || "", time: t.time, groups, state: t.state, note, ctxLine, ctxTime: lastGroupTime });
    if(t.ctx_used){ lastCtxUsed = t.ctx_used; lastCtxWindow = t.ctx_window; } // carry the known context forward to a later running turn
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
function bubble(cls, html, time, dataPos, raw){
  const d = document.createElement("div");
  updateBubble(d, cls, html, time, dataPos, raw);
  return d;
}

function updateBubble(d, cls, html, time, dataPos, raw){
  const meta = metaHTML(time);
  const hasActions = !!(meta || raw), showActions = hasActions && d.classList.contains("show-actions");
  d.className = "msg " + cls + (hasActions ? " has-actions" : "") + (showActions ? " show-actions" : "");
  if(dataPos) d.dataset.pos = String(dataPos); // encoded (turn,block) position — drives read-advance
  else delete d.dataset.pos;
  const copy = raw ? '<button class="mcopy block-copy" title="Копировать сообщение">⧉</button>' : "";
  d.innerHTML = copy + '<div class="body">'+html+'</div>' + meta;
  if(raw) d._raw = raw;
  else delete d._raw;
  return d;
}
// indicator is the per-turn tail dots: null for a settled turn (done/err — err shows its
// error block); animated + ✕ for run; dim note for enq/unknown.
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

// reuseImages swaps a freshly-built node's img.att elements with same-src already-loaded ones from a
// map of discarded nodes' images, so a rebuilt bubble that shows the same image doesn't reload it.
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
  if(it.kind === "bubble") return "bubble:" + (it.key !== undefined ? "e" + it.key : index) + ":" + it.cls;
  return "";
}

function renderSig(it){
  if(it.kind === "turn"){
    return JSON.stringify({
      text: it.text, time: it.time, state: it.state, note: it.note, ctxLine: it.ctxLine,
      groups: it.groups.map(g => ({
        divider: g.divider,
        cls: g.cls, tool: g.tool, time: g.time, startPos: g.startPos, maxPos: g.maxPos,
        joinPrev: !!g.joinPrev, joinNext: !!g.joinNext,
        blocks: (g.blocks || []).map(b => ({ id: b.id, role: b.role, text: b.text, kind: b.kind, time: b.time })),
      })),
    });
  }
  if(it.kind === "bubble") return JSON.stringify({ cls: it.cls, text: it.text, md: it.md, time: it.time, key: it.key });
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

// Each turn child carries a FLIP key (data-flip) AND a content signature (data-csig). The key is an
// independently animatable unit — answer groups are keyed by the durable position of their first
// block, not by content-derived block IDs, so tool-label/text changes patch the same DOM node instead
// of creating an entering replacement. When reading merges bubbles a divider used to split, the
// merged bubble inherits the leading part's position key and stays put. The signature is EVERYTHING
// that determines the child's DOM, so buildTurn can reuse an unchanged child verbatim on the next
// render (no markdown re-parse, no repaint) — only changed blocks and transient indicators update.
function childSig(kind, extra){ return JSON.stringify([kind, extra]); }

// reconcileChildren makes parent's children exactly `desired` (in that order), IN PLACE — reused
// nodes are moved with insertBefore, NEVER detached through a document fragment. Keeping a node
// attached to the live document across a re-render is what stops an <img> attachment from blanking /
// flickering for a frame: a fragment detach (or re-parenting into a brand-new container) forces the
// browser to re-decode the image on re-attach, which is exactly the flicker. An unchanged node that
// keeps its position isn't touched at all.
function reconcileChildren(parent, desired){
  const want = new Set(desired);
  for(const ch of Array.from(parent.children)) if(!want.has(ch)) parent.removeChild(ch);
  let ref = parent.firstChild;
  for(const node of desired){
    if(node === ref){ ref = ref.nextSibling; continue; }
    parent.insertBefore(node, ref); // move an existing child (or insert a new one) before ref
  }
}

// buildTurn renders a user turn, REUSING unchanged child nodes from `old` verbatim so a streaming
// delta or a run→done flip only touches the block that changed — the finished blocks above never
// re-parse or flicker. `old` is the previous turn node, REUSED AS THE CONTAINER and reconciled in
// place (children are never re-parented into a fresh div), or null for a fresh build.
function buildTurn(it, onAbort, old){
  const turn = old || document.createElement("div");
  turn.className = "turn"; turn.dataset.seq = it.seq;
  const reuse = new Map();
  Array.from(turn.children).forEach(ch => {
    if(ch.dataset && ch.dataset.flip && ch.dataset.csig !== undefined) reuse.set(ch.dataset.flip, ch);
  });
  const desired = []; // ordered child nodes; reconciled into `turn` in place at the end
  // put queues a child, reusing the old node verbatim when its signature is unchanged. When a
  // bubble's content changed but its FLIP key is the same, patch the existing .msg in place instead
  // of replacing it: wrapped monospace tool text otherwise visibly blinks during tail re-syncs.
  const put = (key, sig, make, patch) => {
    const o = reuse.get(key);
    if(o && o.dataset.csig === sig){ reuse.delete(key); desired.push(o); return; }
    if(o && patch && patch(o)){
      reuse.delete(key);
      o.dataset.flip = key; o.dataset.csig = sig;
      desired.push(o);
      return;
    }
    const el = make(); el.dataset.flip = key; el.dataset.csig = sig; desired.push(el);
  };
  const patchBubble = (cls, html, time, dataPos, raw) => old => {
    if(!old.classList || !old.classList.contains("msg")) return false;
    updateBubble(old, cls, html, time, dataPos, raw);
    return true;
  };
  const userHTML = mdSafe(it.text), userSig = childSig("u", [it.text, it.time]);
  put("u", userSig, () => bubble("user", userHTML, it.time, undefined, it.text),
    patchBubble("user", userHTML, it.time, undefined, it.text));
  for(const g of it.groups){
    if(g.divider){
      // A fresh in-flow node every render (never reused via put() — it carries
      // no content that could change) — dismissal fades it via fadeOutDivider,
      // then the next render drops it. It still needs data-flip: beginShift/
      // playShift only track children that have one, and without it the line
      // snapped straight to its final layout position on every shift while
      // everything else around it slid smoothly — the "out of sync" look.
      const dv = divider();
      dv.dataset.flip = "divider";
      desired.push(dv);
      continue;
    }
    const fk = "g:" + g.startPos;
    const cls = g.cls + (g.joinPrev ? " join-prev" : "") + (g.joinNext ? " join-next" : "");
    const sig = childSig("g", { cls: g.cls, tool: g.tool, time: g.time, maxPos: g.maxPos, joinPrev: !!g.joinPrev, joinNext: !!g.joinNext,
      blocks: (g.blocks || []).map(b => ({ id: b.id, role: b.role, text: b.text, kind: b.kind, time: b.time })) });
    const html = g.blocks.map(b => g.tool ? esc(b.text || "") : mdSafe(b.text || "")).join(g.tool ? "<br>" : "");
    const raw = g.blocks.map(b => b.text || "").join(g.tool ? "\n" : "\n\n");
    put(fk, sig, () => bubble(cls, html, g.time, g.maxPos, raw),
      patchBubble(cls, html, g.time, g.maxPos, raw));
  }
  // The working/queued dots — the turn's in-progress indicator. INVARIANT: a turn in progress ALWAYS
  // shows this block, the WHOLE time it runs; it disappears only when the turn settles (done/err).
  // Kept a reuse unit so a stream that adds a block above doesn't re-create the animated dots (which
  // would restart the blink) or flicker them.
  if(it.state === "run" || it.state === "enq"){
    put("dots", childSig("dots", [it.state, it.note]), () => indicator(it.state, it.note, onAbort));
  }
  // The context "cut line" is the turn's final element — below the dots while running, and the last
  // line once the dots are gone (it slides up to close the gap).
  if(it.ctxLine){
    const ctxHTML = esc(it.ctxLine), ctxSig = childSig("ctx", [it.ctxLine, it.ctxTime]);
    put("g:ctx:" + it.seq, ctxSig, () => bubble("tool", ctxHTML, it.ctxTime, undefined, it.ctxLine),
      patchBubble("tool", ctxHTML, it.ctxTime, undefined, it.ctxLine));
  }
  reconcileChildren(turn, desired); // apply the child order IN PLACE — reused nodes stay attached
  return turn;
}

function buildItem(it, onAbort){
  if(it.kind === "divider") return divider();
  if(it.kind === "bubble") return bubble(it.cls, it.md ? mdSafe(it.text) : esc(it.text), it.time, undefined, it.text);
  return buildTurn(it, onAbort, null);
}

export function paint(col, items, onAbort){
  const nodes = reusableNodes(col);
  const desired = []; // ordered final nodes, reconciled into `col` in place (no fragment detach)
  const fresh = [];   // freshly-built nodes that may hold NEW <img> elements to reconnect
  items.forEach((it, index) => {
    const key = renderKey(it, index);
    const sig = renderSig(it);
    const old = key && nodes.get(key);
    if(old && old.dataset.renderSig === sig){
      nodes.delete(key); // a duplicate key later must build fresh, not steal this node
      desired.push(old); // reuse verbatim, in place
      return;
    }
    // A changed/new turn is reconciled INTO its old container (reusing unchanged children verbatim, no
    // re-parse); other kinds are built fresh. Consume the old key either way.
    let built, reusedContainer = false;
    if(it.kind === "turn"){
      const oc = old && old.classList && old.classList.contains("turn") ? old : null;
      built = buildTurn(it, onAbort, oc);
      reusedContainer = !!oc; // its children (incl. images) were reused in place — no new imgs to swap
    } else {
      built = buildItem(it, onAbort);
    }
    if(old) nodes.delete(key);
    const stamped = stamp(built, key, sig);
    desired.push(stamped);
    if(!reusedContainer) fresh.push(stamped);
  });
  // Preserve already-loaded images: swap a NEW img.att in a freshly-built node with the same-src node
  // from a DISCARDED old node (the un-consumed `nodes` — about to be removed), never from a still-
  // present reused node. In-place node reuse already keeps most images attached; this covers a node
  // that was genuinely rebuilt (e.g. a bubble split/merge) but shows the same image.
  if(fresh.length && nodes.size){
    const bySrc = new Map();
    nodes.forEach(node => {
      if(!node.querySelectorAll) return;
      node.querySelectorAll("img.att[src]").forEach(img => {
        const src = img.getAttribute("src"); if(!src) return;
        let list = bySrc.get(src); if(!list){ list = []; bySrc.set(src, list); }
        list.push(img);
      });
    });
    if(bySrc.size) fresh.forEach(node => reuseImages(node, bySrc));
  }
  reconcileChildren(col, desired);
}

export function renderSession(col, turns, unreadAfter, onAbort, holdSplits, joinHeldSplits){
  paint(col, renderModel(turns, unreadAfter, holdSplits, joinHeldSplits), onAbort);
}

// --- smooth live updates (FLIP) ---
// beginShift snapshots visual positions before a LIVE re-render; playShift then slides every
// surviving unit from its old position to the new one and gives genuinely new nodes a short entry
// animation. A FLIP *unit* is what actually moves: a standalone keyed bubble, or a CHILD of a .turn
// (user bubble / answer group / indicator, keyed turnKey|data-flip) — the .turn container itself is
// never transformed, so parent and child shifts cannot compound, and an in-turn divider collapse
// animates block-level. Transforms only — layout and all scroll maths stay exact. Reduced motion
// disables it. Dismissing the unread line is a SEPARATE first phase (fadeOutDivider): the line fades
// in place, THEN a follow-up render removes it and this FLIP collapses the gap.
export const DIVIDER_FADE_MS = 300; // .readline.leaving lineout duration in app.css
const SHIFT_MS = 180, SHIFT_CAP = 800;

function reducedMotion(){
  return typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
}

export function beginShift(col){
  if(reducedMotion()) return null;
  const units = new Map(), keys = new Set(), holdSplits = new Map();
  Array.from(col.children).forEach(node => {
    const key = node.dataset && node.dataset.renderKey;
    if(!key) return;
    keys.add(key);
    if(node.classList.contains("turn")){
      Array.from(node.children).forEach(ch => {
        const fk = ch.dataset && ch.dataset.flip;
        if(fk) units.set(key + "|" + fk, ch.getBoundingClientRect().top); // visual pos, mid-animation included
      });
      const seq = Number(node.dataset.seq);
      if(Number.isFinite(seq)){
        const children = Array.from(node.children);
        const splits = new Set();
        for(let i = 0; i < children.length; i++){
          const ch = children[i];
          if(!ch.classList || !ch.classList.contains("readline")) continue;
          for(let j = i + 1; j < children.length; j++){
            const fk = children[j].dataset && children[j].dataset.flip;
            const m = fk && fk.match(/^g:(\d+)$/);
            if(!m) continue;
            const p = Number(m[1]);
            splits.add(p);
            break;
          }
        }
        if(splits.size) holdSplits.set(seq, splits);
      }
    } else {
      units.set(key, node.getBoundingClientRect().top);
    }
  });
  col.querySelectorAll(".enter").forEach(n => n.classList.remove("enter"));
  return { units, keys, holdSplits, hadAny: col.children.length > 0 };
}

export function playShift(col, snap){
  if(!snap) return 0;
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
  return (shifts.length || fresh.length) ? SHIFT_MS : 0;
}

// fadeOutDivider dismisses the unread line as its FIRST phase: the real in-flow .readline node fades
// in place (opacity → 0) exactly where it sits, so it scrolls with the messages and needs no ghost or
// coordinates. commitLive waits DIVIDER_FADE_MS, then a normal render removes it and playShift
// collapses the gap. Returns false (no node / reduced motion) so the caller collapses immediately.
export function fadeOutDivider(col){
  if(!col || reducedMotion()) return false;
  const dv = col.querySelector(".readline");
  if(!dv) return false;
  dv.classList.add("leaving");
  return true;
}
