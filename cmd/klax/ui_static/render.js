// render.js — turns the read model into DOM. renderModel() is PURE (no DOM, no markdown):
// it computes ordered render items from turn.state alone — no array-index surgery, no
// runningTurn/queuedTurns guessing. A user turn is ONE {kind:"turn"} unit (its user bubble
// + grouped answer blocks + state indicator render inside one <div class="turn" data-seq>);
// standalone notices and the unread divider are their own items. paint() is the thin DOM
// layer (exercised end-to-end by the Step-4 browser harness). Markdown + capability image
// refs come from markdown.js.

import { mdSafe, esc, fmtTime } from "./markdown.js";

function blockCls(role){ return role === "tool" ? "tool" : role === "error" ? "error" : role === "system" ? "system" : "assistant"; }

// renderModel computes the ordered render items for one session. PURE and unit-testable.
// The unread divider stays turn-aware: user bubbles are the human's own messages and do
// not count as unread; unread answer blocks land inside the turn at the exact block
// boundary, even when they have the same role as the preceding already-read block.
export function renderModel(turns, unreadAfter){
  const items = [];
  let queuePos = 0, divided = false;
  const placeDivider = () => { if(!divided && unreadAfter !== undefined){ items.push({ kind: "divider" }); divided = true; } };
  const isUnread = es => es !== undefined && unreadAfter !== undefined && es > unreadAfter;
  for(const t of (turns || [])){
    if(t.role !== "user"){
      if(isUnread(t.eventSeq)) placeDivider();
      if(t.kind === "compact") items.push({ kind: "bubble", cls: "system", text: "🗜 контекст свёрнут", md: false, time: t.time });
      else if(t.role === "notice") items.push({ kind: "bubble", cls: "notice", text: t.text || "", md: false, time: t.time });
      else items.push({ kind: "bubble", cls: (t.kind === "error" || t.role === "error") ? "error" : t.role === "system" ? "system" : "assistant", text: t.text || "", md: true, time: t.time });
      continue;
    }
    const groups = [];
    let i = 0;
    while(i < (t.blocks || []).length){
      if(isUnread(t.blocks[i].eventSeq) && !divided && unreadAfter !== undefined){
        groups.push({ divider: true });
        divided = true;
        continue;
      }
      const role = t.blocks[i].role, blocks = [];
      while(i < t.blocks.length && t.blocks[i].role === role){
        if(isUnread(t.blocks[i].eventSeq) && !divided && unreadAfter !== undefined && blocks.length > 0) break;
        blocks.push(t.blocks[i]); i++;
      }
      groups.push({ cls: blockCls(role), blocks, tool: role === "tool", time: blocks.length ? blocks[blocks.length - 1].time : undefined });
    }
    if(t.state === "enq") queuePos++;
    items.push({ kind: "turn", seq: t.seq, text: t.text || "", time: t.time, groups, state: t.state, note: t.state === "enq" ? "в очереди · " + queuePos : undefined });
  }
  return items;
}

// --- DOM layer (thin) ---
const DOTS = '<span class="dots"><span></span><span></span><span></span></span>';

function divider(){
  const d = document.createElement("div");
  d.className = "readline"; d.innerHTML = "<span>новые сообщения</span>";
  return d;
}

function timeMeta(time){
  if(!time) return "";
  const u = typeof time === "number" ? time : Date.parse(time);
  return u ? '<span class="meta">'+esc(fmtTime(u))+'</span>' : "";
}
function bubble(cls, html, time){
  const meta = timeMeta(time);
  const d = document.createElement("div");
  d.className = "msg " + cls + (meta ? " hasmeta" : "");
  d.innerHTML = '<div class="body">'+html+'</div>' + meta;
  return d;
}
// indicator is the per-turn tail dots: null for a settled turn (done/err — err shows its
// error block); animated + ✕ for run; dim 'в очереди · N' for enq.
function indicator(state, note, onAbort){
  if(state === "done" || state === "err" || state === undefined) return null;
  const d = document.createElement("div");
  const animated = state === "run";
  const abortable = state === "run" || state === "enq";
  d.className = "msg assistant typing" + (animated ? "" : " queued");
  d.innerHTML = DOTS + (note ? '<span class="qnote">'+esc(note)+'</span>' : "") + (abortable ? '<button class="stop" title="Прервать">✕</button>' : "");
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
  if(it.kind === "bubble") return "bubble:" + index + ":" + it.cls;
  return "";
}

function renderSig(it){
  if(it.kind === "turn"){
    return JSON.stringify({
      text: it.text, time: it.time, state: it.state, note: it.note,
      groups: it.groups.map(g => ({
        divider: g.divider,
        cls: g.cls, tool: g.tool, time: g.time,
        blocks: (g.blocks || []).map(b => ({ id: b.id, role: b.role, text: b.text, time: b.time })),
      })),
    });
  }
  if(it.kind === "bubble") return JSON.stringify({ cls: it.cls, text: it.text, md: it.md, time: it.time });
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

function buildItem(it, onAbort){
  if(it.kind === "divider") return divider();
  if(it.kind === "bubble") return bubble(it.cls, it.md ? mdSafe(it.text) : esc(it.text), it.time);
  const turn = document.createElement("div");
  turn.className = "turn"; turn.dataset.seq = it.seq;
  turn.appendChild(bubble("user", mdSafe(it.text), it.time));
  for(const g of it.groups){
    if(g.divider){ turn.appendChild(divider()); continue; }
    const html = g.blocks.map(b => g.tool ? esc(b.text || "") : mdSafe(b.text || "")).join(g.tool ? "<br>" : "");
    turn.appendChild(bubble(g.cls, html, g.time));
  }
  const ind = indicator(it.state, it.note, onAbort);
  if(ind) turn.appendChild(ind);
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
