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
// The unread divider is TURN-local (§B3): it lands before a turn iff the turn's accept
// event OR any of its blocks is unread (eventSeq > unreadAfter) — never between a user
// bubble and its own answer.
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
    if(isUnread(t.eventSeq) || (t.blocks || []).some(b => isUnread(b.eventSeq))) placeDivider();
    const groups = [];
    let i = 0;
    while(i < (t.blocks || []).length){
      const role = t.blocks[i].role, blocks = [];
      while(i < t.blocks.length && t.blocks[i].role === role){ blocks.push(t.blocks[i]); i++; }
      groups.push({ cls: blockCls(role), blocks, tool: role === "tool", time: blocks.length ? blocks[blocks.length - 1].time : undefined });
    }
    if(t.state === "enq") queuePos++;
    items.push({ kind: "turn", seq: t.seq, text: t.text || "", time: t.time, groups, state: t.state, note: t.state === "enq" ? "в очереди · " + queuePos : undefined });
  }
  return items;
}

// --- DOM layer (thin) ---
const DOTS = '<span class="dots"><span></span><span></span><span></span></span>';

function timeMeta(time){
  if(!time) return "";
  const u = typeof time === "number" ? time : Date.parse(time);
  return u ? '<span class="meta">'+esc(fmtTime(u))+'</span>' : "";
}
function bubble(cls, html, time){
  const d = document.createElement("div");
  d.className = "msg " + cls;
  d.innerHTML = '<div class="body">'+html+'</div>' + timeMeta(time);
  return d;
}
// indicator is the per-turn tail dots: null for a settled turn (done/err — err shows its
// error block); animated + ✕ for run; animated for the just-sent 'sending'; dim 'в
// очереди · N' for enq.
function indicator(state, note, onAbort){
  if(state === "done" || state === "err" || state === undefined) return null;
  const d = document.createElement("div");
  const animated = state === "run" || state === "sending";
  d.className = "msg assistant typing" + (animated ? "" : " queued");
  d.innerHTML = DOTS + (note ? '<span class="qnote">'+esc(note)+'</span>' : "") + (state === "run" ? '<button class="stop" title="Прервать">✕</button>' : "");
  if(state === "run" && onAbort){ const b = d.querySelector(".stop"); if(b) b.addEventListener("click", onAbort); }
  return d;
}

export function paint(col, items, onAbort){
  col.innerHTML = "";
  for(const it of items){
    if(it.kind === "divider"){
      const d = document.createElement("div");
      d.className = "readline"; d.innerHTML = "<span>новые сообщения</span>";
      col.appendChild(d);
    } else if(it.kind === "bubble"){
      col.appendChild(bubble(it.cls, it.md ? mdSafe(it.text) : esc(it.text), it.time));
    } else { // per-turn container
      const turn = document.createElement("div");
      turn.className = "turn"; turn.dataset.seq = it.seq;
      turn.appendChild(bubble("user", mdSafe(it.text), it.time));
      for(const g of it.groups){
        const html = g.blocks.map(b => g.tool ? esc(b.text || "") : mdSafe(b.text || "")).join(g.tool ? "<br>" : "");
        turn.appendChild(bubble(g.cls, html, g.time));
      }
      const ind = indicator(it.state, it.note, onAbort);
      if(ind) turn.appendChild(ind);
      col.appendChild(turn);
    }
  }
}

export function renderSession(col, turns, unreadAfter, onAbort){
  paint(col, renderModel(turns, unreadAfter), onAbort);
}
