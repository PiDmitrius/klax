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
// as unread; standalone cosmetic rows (compact/notice) are not counted and just flow to the right
// side of the divider by document order; unread answer blocks land inside the turn at the exact
// block boundary. `watermark` is the encoded read position (pos()); undefined ⇒ no divider.
// `contextHint` is the current session-level usage snapshot, used only as a live fallback while
// the running turn has not yet produced turn-local usage.
// `holdSplits` preserves group boundaries that used to be separated by the unread divider for one
// live frame after the divider disappears. That lets the line fade out before the two bubble pieces
// merge back into one.
export function renderModel(turns, watermark, contextHint, holdSplits, joinHeldSplits){
  const items = [];
  let queuePos = 0, divided = false;
  const has = watermark !== undefined;
  const unread = p => has && p > watermark;
  for(const t of (turns || [])){
    if(t.role !== "user"){
      // `key` is the standalone's live eventSeq (render-key stability only); it carries no data-pos
      // so it never drives read-advance, and it is not counted as unread.
      if(t.kind === "compact") items.push({ kind: "bubble", cls: "system", text: "🗜 контекст свёрнут", md: false, time: t.time, key: t.eventSeq });
      else if(t.role === "notice") items.push({ kind: "bubble", cls: "notice", text: t.text || "", md: false, time: t.time, key: t.eventSeq });
      else items.push({ kind: "bubble", cls: (t.kind === "error" || t.role === "error") ? "error" : t.role === "system" ? "system" : "assistant", text: t.text || "", md: true, time: t.time, key: t.eventSeq });
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
    // the gap, still the bottom line. A running turn falls back to the current session context
    // snapshot until turn-local usage appears; do not store that snapshot on the turn itself, or a
    // copied hint can outlive the fresh session strip and render stale values under the dots.
    // It is NOT a group — buildItem renders it after the dots indicator.
    const finalCtx = contextText(t.ctx_used, t.ctx_window);
    const liveCtx = contextText(contextHint && contextHint.used, contextHint && contextHint.window);
    const ctxLine = (t.state === "done" || t.state === "err") ? finalCtx
      : t.state === "run" ? (finalCtx || liveCtx)
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
function bubble(cls, html, time, dataPos, raw){
  const d = document.createElement("div");
  updateBubble(d, cls, html, time, dataPos, raw);
  return d;
}

function updateBubble(d, cls, html, time, dataPos, raw){
  const meta = metaHTML(time);
  d.className = "msg " + cls + (meta ? " hasmeta" : "");
  if(dataPos) d.dataset.pos = String(dataPos); // encoded (turn,block) position — drives read-advance
  else delete d.dataset.pos;
  const copy = raw ? '<button class="mcopy" title="Копировать сообщение">⧉</button>' : "";
  d.innerHTML = copy + '<div class="body">'+html+'</div>' + meta;
  if(raw) d._raw = raw;
  else delete d._raw;
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

// buildTurn renders a user turn, REUSING unchanged child nodes from `old` verbatim so a streaming
// delta or a run→done flip only touches the block that changed — the finished blocks above never
// re-parse or flicker. `old` is the previous turn node (or null for a fresh build).
function buildTurn(it, onAbort, old){
  const turn = document.createElement("div");
  turn.className = "turn"; turn.dataset.seq = it.seq;
  const reuse = new Map();
  if(old) Array.from(old.children).forEach(ch => {
    if(ch.dataset && ch.dataset.flip && ch.dataset.csig !== undefined) reuse.set(ch.dataset.flip, ch);
  });
  // put appends a child, reusing the old node verbatim when its signature is unchanged. When a
  // bubble's content changed but its FLIP key is the same, patch the existing .msg in place instead
  // of replacing it: wrapped monospace tool text otherwise visibly blinks during tail re-syncs.
  const put = (key, sig, make, patch) => {
    const o = reuse.get(key);
    if(o && o.dataset.csig === sig){ reuse.delete(key); turn.appendChild(o); return; }
    if(o && patch && patch(o)){
      reuse.delete(key);
      o.dataset.flip = key; o.dataset.csig = sig;
      turn.appendChild(o);
      return;
    }
    const el = make(); el.dataset.flip = key; el.dataset.csig = sig; turn.appendChild(el);
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
    if(g.divider){ turn.appendChild(divider()); continue; } // divider: cheap + tracked via snap.divider, not a reuse unit
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
  return turn;
}

function buildItem(it, onAbort){
  if(it.kind === "divider") return divider();
  if(it.kind === "bubble") return bubble(it.cls, it.md ? mdSafe(it.text) : esc(it.text), it.time, undefined, it.text);
  return buildTurn(it, onAbort, null);
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
    // A changed/new turn is rebuilt REUSING its unchanged child nodes from the old turn (verbatim, no
    // re-parse); other item kinds are built fresh. Consume the old key either way.
    const built = it.kind === "turn" ? buildTurn(it, onAbort, old || null) : buildItem(it, onAbort);
    if(old) nodes.delete(key);
    frag.appendChild(stamp(built, key, sig));
  });
  const images = reusableImages(col);
  reuseImages(frag, images);
  col.replaceChildren(frag);
}

export function renderSession(col, turns, unreadAfter, onAbort, contextHint, holdSplits, joinHeldSplits){
  paint(col, renderModel(turns, unreadAfter, contextHint, holdSplits, joinHeldSplits), onAbort);
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
const SHIFT_MS = 180, SHIFT_CAP = 800, DIVIDER_FADE_MS = 300; // DIVIDER_FADE_MS matches .readline.ghost lineout in app.css

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
  const dv = col.querySelector(".readline");
  return { units, keys, holdSplits, divider: dv ? dv.getBoundingClientRect() : null, hadAny: col.children.length > 0 };
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
  // When the "непрочитанные сообщения" line vanishes, split the motion into two phases so
  // the sliding blocks never cross the still-visible line: the ghost fades out FIRST, and
  // only then does the gap collapse. A transition-delay equal to the fade holds every shifted
  // block at its old position while the line fades, then slides it up. A plain reflow (no
  // divider gone) has zero delay and collapses immediately, exactly as before.
  const dividerGone = snap.divider && !col.querySelector(".readline");
  const collapseDelay = dividerGone ? DIVIDER_FADE_MS : 0;
  if(shifts.length){
    void col.offsetHeight; // commit the start positions before transitioning
    shifts.forEach(([el]) => {
      el.style.transition = "transform " + SHIFT_MS + "ms ease-out" + (collapseDelay ? " " + collapseDelay + "ms" : "");
      el.style.transform = "";
      el.addEventListener("transitionend", () => { el.style.transition = ""; }, { once: true });
    });
  }
  if(dividerGone) fadeDividerGhost(snap.divider);
  if(dividerGone) return DIVIDER_FADE_MS + (shifts.length ? SHIFT_MS : 0);
  return (shifts.length || fresh.length) ? SHIFT_MS : 0;
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
