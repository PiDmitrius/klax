// app.js — bootstrap + wiring. Owns the model, the active-session render flow, and the tail host
// (per-session content cursors + the notice cursor + onAffected); ties compose + tabs together.
// The whole old state machine (runningTurn/doneTurns/queuedTurns/tmpTurn/renderedPending/readMark/
// insertAnswer/breakMerge) is gone — a turn's truth is model turn.state.

import { TurnModel } from "./model.js";
import { renderSession, beginShift, playShift, fadeOutDivider, DIVIDER_FADE_MS, pos, parsePos, decodePos } from "./render.js";
import { tailLoop } from "./events.js";
import { api, getToken, setToken } from "./base.js";
import { selectionInLog } from "./scroll.js";
import { initCompose, saveDraft, loadDraft, dropDraft } from "./compose.js";
import { initTabs, reconcileSessions, renderTabs } from "./tabs.js";
import { injectEmojiFont } from "./emoji.js";

const model = new TurnModel();
const loaded = {};        // created -> transcript loaded?
const readThrough = {};   // created -> encoded (turn,block) read watermark (pos()); undefined until seeded
const unreadJump = {};    // created -> one-shot scroll to the unread divider
const readGraceUntil = {}, readGraceTimer = {};
const readReportTimer = {}; // created -> pending POST /api/read debounce timer
const READ_GRACE_MS = 1600;
let active = 0;
const tailCursors = {};                // created -> "<turn>.<block>.<state>.<trail>[.<head>]" durable content cursor
let noticeCursor = "";                 // ring cursor for transient notices (tailLoop)
let sessRev = 0;                       // last session-strip revision rendered (tailLoop; server returns it early on a strip change)
let stick = true, pendingRender = false, readOnScroll = true;
let liveRenderRAF = 0, liveRenderCreated = 0;
// Live DOM commits are serialized so streamed blocks never animate on top of each other.
// While an entrance/FLIP is in flight the model keeps updating, but the DOM commit is
// deferred and COALESCED: everything that arrived during the window then appears as ONE
// block growing out of the dots, instead of a cascade of overlapping slide-ins. Only the
// animation is throttled (COMMIT_MS, a hair over the 180ms entrance) — the data stays live.
const COMMIT_MS = 200;
const MERGE_JOIN_MS = 180;
let liveBusy = false, liveDirty = false, liveGateTimer = 0;
let sessionList = []; // last /api/sessions list — for hash-change validity + lookups
const offsetFor = {}, moreFor = {}; // created -> first-loaded turn index + has-older-history flag (pagination)
const loadingOlder = {}; // created -> a loadOlder() is in flight (guards the auto-load-on-scroll + the initial fill)
// Timeline window (anchored on the "непрочитанные сообщения" line = the read watermark). Measured in
// BUBBLES (a user turn = its message bubble + one per answer block/tool call; a standalone = 1) — the
// unit the user sees, so one big turn counts as many, not one. CAP is the loadOlder page in TURNS
// (server pagination unit).
//   - everything at/below the line (all unread) is ALWAYS kept;
//   - ≥ KEEP_ABOVE bubbles of read context are kept above the line (rounded up to a turn boundary);
//   - older rows evict from the top once total bubbles exceed WIN_MAX (never the viewport-to-bottom range);
//   - the line is guaranteed loaded (ensureLineLoaded pulls older pages if it sits above the first page);
//   - older history auto-loads CAP turns at a time when the user scrolls to the top of what is loaded.
const KEEP_ABOVE = 60, CAP = 20, WIN_MAX = 120;
const scrollTopFor = {}; // created -> last scrollTop, so tab switches do not snap by a pixel
const watchedImages = new WeakSet();
let composerH = 0; // observed #composer height — the bottom-anchor baseline (see start())

function setComposerH(h){
  composerH = h || 0;
  const wrap = document.getElementById("logwrap");
  if(wrap) wrap.style.setProperty("--composer-h", composerH + "px");
}
// syncComposerH re-baselines the composer observer after a deliberate height change
// (tab switch swapping drafts), so only typing/chips growth shifts the bottom padding.
function syncComposerH(){ const c = document.getElementById("composer"); if(c) setComposerH(c.offsetHeight); }

function logcol(){ return document.getElementById("logcol"); }
function getActive(){ return active; }

// stateCode mirrors the server (readmodel.go): the tail cursor carries the boundary turn's state
// code so a pure enq→run transition (no new block) still advances the cursor and re-delivers the
// turn once — otherwise the bubble stays "queued" until the first block or a reload.
function stateCode(s){ return s === "run" ? "r" : s === "done" ? "d" : s === "err" ? "x" : "e"; }

// tailPos is the durable "<turn>.<block>.<state>.<trail>[.<head>]" content cursor to resume the live
// tail from — it MIRRORS the server's tailCursor. The anchor (turn/block/state) is the OLDEST
// unsettled turn (enq/run) so a still-running turn behind a newer queued one keeps getting its blocks
// + completion; `head` (the newest turn) is appended only when it is past the anchor, so an
// already-seen queued turn is not re-flagged new. block -1 for a turn with no answer blocks yet.
function tailPos(rows){
  let head = 0, turn = 0, block = -1, state = "", trail = 0, anchored = false;
  for(const t of (rows || [])){
    // `seq > 0` mirrors the server's tailCursor (only positive durable seqs anchor); a legacy negative
    // synthetic seq counts as trailing, exactly like a standalone, so client seed == server cursor.
    if(t.role === "user" && t.seq > 0){
      head = t.seq; trail = 0;
      if(!anchored){ turn = t.seq; block = (t.blocks || []).length - 1; state = t.state; anchored = t.state === "enq" || t.state === "run"; }
    }
    else trail++;
  }
  const base = turn + "." + block + "." + stateCode(state) + "." + trail;
  return (anchored && turn !== head) ? base + "." + head : base;
}
function sameSession(a, b){ return String(a) === String(b); }
function documentVisible(){ return typeof document === "undefined" || document.visibilityState !== "hidden"; }
function clearReadGrace(created){
  if(!created) return;
  delete readGraceUntil[created];
  if(readGraceTimer[created]){
    clearTimeout(readGraceTimer[created]);
    delete readGraceTimer[created];
  }
}
function inReadGrace(created){ return !!created && (readGraceUntil[created] || 0) > Date.now(); }
function startReadGrace(created){
  if(!created) return;
  readGraceUntil[created] = Date.now() + READ_GRACE_MS;
  if(readGraceTimer[created]) clearTimeout(readGraceTimer[created]);
  readGraceTimer[created] = setTimeout(() => {
    delete readGraceTimer[created];
    if(inReadGrace(created)) return;
    clearReadGrace(created);
    if(active === created && documentVisible() && stick && rawUnreadCount(created) > 0){
      markRead(created, true);
      renderTabs(active);
      commitLive(created); // the unread line fades out, messages close the gap, then split bubbles merge.
    }
  }, READ_GRACE_MS + 40);
}
function markRead(created, force){
  if(!created) return false;
  if(!force && inReadGrace(created)) return false;
  const visualChange = rawUnreadCount(created) > 0 || unreadJump[created] !== undefined || readGraceUntil[created] !== undefined;
  const prev = readThrough[created] || 0;
  const next = Math.max(prev, modelMaxPos(created));
  readThrough[created] = next;
  delete unreadJump[created];
  clearReadGrace(created);
  readOnScroll = true;
  if(next !== prev) reportRead(created); // persist ONLY when the watermark actually advanced — no redundant /api/read
  return visualChange;
}
// modelMaxPos is the (turn,block) position of the LAST answer block currently in the model —
// "read up to now". A later block (same turn next index, or a new turn) sorts after it, so a new
// arrival reads as unread. Empty (answerless) turns contribute nothing; their first block, when it
// lands, is unread by its higher turn_seq anyway.
function modelMaxPos(created){
  let max = 0;
  for(const t of model.turns(created)){
    if(t.role === "user" && t.seq !== undefined){
      const nb = (t.blocks || []).length;
      if(nb > 0){ const p = pos(t.seq, nb - 1); if(p > max) max = p; }
    }
  }
  return max;
}
// reportRead pushes the durable read watermark to the server (POST /api/read), debounced so a
// scroll burst coalesces to one request. flushRead sends it immediately — used on tab-hide, before
// the tab can freeze; `keepalive` lets that request outlive a backgrounding/close. The server
// raises the watermark monotonically, so a late or duplicate report is a harmless no-op.
function reportRead(created){
  if(!created || readThrough[created] === undefined) return;
  if(readReportTimer[created]) return;
  readReportTimer[created] = setTimeout(() => { delete readReportTimer[created]; flushRead(created); }, 400);
}
function flushRead(created){
  if(!created || readThrough[created] === undefined) return;
  if(readReportTimer[created]){ clearTimeout(readReportTimer[created]); delete readReportTimer[created]; }
  const { turn, block } = decodePos(readThrough[created]);
  api("/api/read", { method: "POST", keepalive: true, headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: created, turn, block }) }).catch(()=>{});
}
function jumpToUnread(created){ if(created){ unreadJump[created] = true; startReadGrace(created); } }
function focusComposer(){
  const input = document.getElementById("input");
  if(input && documentVisible()) input.focus({ preventScroll: true });
}
// settledDistance measures how far the view is from the SETTLED bottom of the timeline.
// #logcol.offsetHeight is layout geometry: unlike log.scrollHeight it is NOT inflated by
// the transient FLIP transforms (a unit mid-slide extends the scrollable overflow), so
// stick/pin decisions taken during a 180ms animation stay correct.
function settledDistance(log){
  const col = logcol();
  // The composer's height is a transparent bottom border on #log (see app.css), so it is
  // NOT scrollable content — the settled height is just the column, and clientHeight already
  // excludes the border. (That is why there is no +composerH here anymore.)
  const h = col ? col.offsetHeight : log.scrollHeight;
  return h - log.scrollTop - log.clientHeight;
}
function stickToBottom(){
  const sc = document.getElementById("log");
  const col = logcol();
  // pin to the settled bottom, not the animation-inflated scrollHeight — pinning to the
  // inflated max overshoots, then snaps back when the slide finishes. col.offsetHeight (no
  // +composerH) because the composer is a bottom border on #log, not scrollable padding.
  if(sc) sc.scrollTop = Math.max(0, (col ? col.offsetHeight : sc.scrollHeight) - sc.clientHeight);
  toggleToBottom();
}
function rememberScroll(created){
  const log = document.getElementById("log");
  if(created && log) scrollTopFor[created] = log.scrollTop;
}
function restoreScroll(created){
  const log = document.getElementById("log");
  if(!created || !log || scrollTopFor[created] === undefined) return;
  log.scrollTop = Math.min(scrollTopFor[created], Math.max(0, log.scrollHeight - log.clientHeight));
  stick = settledDistance(log) < 80;
  toggleToBottom();
}
function watchInlineImages(col){
  col.querySelectorAll("img.att").forEach(img => {
    if(watchedImages.has(img)) return;
    watchedImages.add(img);
    const settle = () => { if(stick) stickToBottom(); };
    if(!img.complete){
      img.addEventListener("load", settle, { once: true });
      img.addEventListener("error", settle, { once: true });
    }
  });
}

function applyTheme(t){
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem("klax_theme2", t); } catch(e){}
  const b = document.getElementById("theme"); if(b) b.textContent = t === "dark" ? "☀️" : "🌙";
}

function noMotion(){ return { motionMS: 0, mergeHeldSplits: false, holdSplits: null, stickAfter: false }; }

// rerender(created, live): live=true marks event-driven updates — they run through the
// FLIP snapshot (render.js beginShift/playShift) so new messages slide in and a vanished
// unread divider collapses smoothly instead of jerking the screen. Structural renders
// (tab switch, transcript load, pagination, foregrounding) stay instant — their scroll
// repositioning must not be animated over.
function rerender(created, live, opts){
  opts = opts || {};
  if(created !== active) return noMotion();
  if(!live && liveBusy && created === active && !opts.forceStructural){
    liveDirty = true;
    return noMotion();
  }
  if(!live){ // a structural render (tab switch, load, foreground) supersedes any queued live animation
    if(liveRenderRAF){ cancelAnimationFrame(liveRenderRAF); liveRenderRAF = 0; liveRenderCreated = 0; }
    if(liveGateTimer){ clearTimeout(liveGateTimer); liveGateTimer = 0; }
    liveBusy = false; liveDirty = false;
  }
  const col = logcol();
  if(!col) return noMotion();
  if(selectionInLog(col)){ pendingRender = true; return noMotion(); } // don't collapse a live selection
  const log = document.getElementById("log");
  const anchorLive = !!(live && log);
  const beforeTop = anchorLive ? log.scrollTop : 0;
  const beforeColH = anchorLive ? col.offsetHeight : 0;
  const hadDivider = anchorLive && !!col.querySelector(".readline");
  const snap = live ? beginShift(col) : null;
  const holdSplits = opts.holdSplits || (!opts.noHoldSplits && hadDivider && rawUnreadCount(active) === 0 && snap && snap.holdSplits && snap.holdSplits.size ? snap.holdSplits : null);
  renderSession(col, model.turns(active), readThrough[active], abortActive, sessionContextHint(active), holdSplits, !!opts.joinHeldSplits);
  watchInlineImages(col);
  if(moreFor[active]){ // older history exists → a "load earlier" button at the top
    const m = document.createElement("button");
    m.id = "more"; m.textContent = "↑ Загрузить раньше";
    m.addEventListener("click", () => loadOlder(active, true)); // showTop: reveal the loaded rows
    col.insertBefore(m, col.firstChild);
  }
  const dividerGone = hadDivider && !col.querySelector(".readline");
  if(unreadJump[active] && rawUnreadCount(active) > 0){
    const dv = col.querySelector(".readline");
    if(dv){
      readOnScroll = false;
      dv.scrollIntoView({ block: "start" });
      stick = false;
      delete unreadJump[active];
    }
  } else if(dividerGone){
    // At the bottom, playShift owns the visible sequence: line fades, blocks collapse, split bubbles
    // join. Away from the bottom (or with reduced motion), preserve the reader's viewport instead:
    // the divider may be off-screen, so moving visible content for it is a regression.
    if(!stick || !snap) log.scrollTop = Math.max(0, beforeTop + (col.offsetHeight - beforeColH));
  } else if(stick) stickToBottom();
  toggleToBottom();
  const motionMS = snap ? playShift(col, snap) : 0; // after scroll decisions: deltas = exact visual shifts
  return {
    motionMS,
    mergeHeldSplits: !!(dividerGone && holdSplits),
    holdSplits,
    stickAfter: !!(dividerGone && stick && motionMS),
  };
}

function rerenderStructural(created, force){
  return rerender(created, false, { forceStructural: !!force });
}

// scheduleLiveRerender funnels every live content update through the serialization gate.
// Gate OPEN → commit on the next frame (same-frame events still coalesce via the rAF). Gate
// CLOSED (an entrance is playing) → just mark dirty; commitLive's timer flushes the
// accumulated changes as ONE further animation the moment the gate reopens. The model was
// already patched before we got here, so nothing waits on the DOM — only the animation does.
function scheduleLiveRerender(created){
  if(created !== active) return;
  if(liveBusy){ liveDirty = true; return; } // an animation is in flight — accumulate, don't stack
  liveRenderCreated = created;
  if(liveRenderRAF) return;
  liveRenderRAF = requestAnimationFrame(() => {
    const c = liveRenderCreated;
    liveRenderRAF = 0;
    liveRenderCreated = 0;
    if(c === active) commitLive(c);
  });
}

// commitLive paints one animated frame and closes the gate for COMMIT_MS. Whatever arrives
// during that window sets liveDirty and is flushed as a single further animation when the
// gate reopens — so a burst of streamed blocks queues into clean, non-overlapping grows.
function commitLive(created){
  if(liveBusy){ liveDirty = true; return; } // an animation is in flight — accumulate; openGate flushes it as one further animation
  liveBusy = true;
  liveDirty = false;
  if(liveGateTimer) clearTimeout(liveGateTimer);
  const openGate = () => {
    liveGateTimer = 0;
    liveBusy = false;
    if(liveDirty && active) scheduleLiveRerender(active);
  };
  // Phase 2+: remove the (now-faded) unread line, collapse the gap, then merge any bubble the line split.
  const collapseAndMerge = () => {
    const first = rerender(created, true);
    liveGateTimer = setTimeout(() => {
      if(first.mergeHeldSplits && active === created){
        const joined = rerender(created, true, { holdSplits: first.holdSplits, joinHeldSplits: true });
        const joinWait = Math.max(MERGE_JOIN_MS, joined.motionMS || 0);
        liveGateTimer = setTimeout(() => {
          const merged = rerender(created, true, { noHoldSplits: true });
          if(first.stickAfter && active === created && stick) stickToBottom();
          liveGateTimer = setTimeout(openGate, Math.max(COMMIT_MS, merged.motionMS || 0));
        }, joinWait);
        return;
      }
      if(first.stickAfter && active === created && stick) stickToBottom();
      openGate();
    }, Math.max(COMMIT_MS, first.motionMS || 0));
  };
  // Phase 1 — ONLY when the read line is being dismissed (nothing left unread but the line is still in
  // the DOM): the real in-flow .readline fades out in place where it sits (scrolls with the messages,
  // no ghost). The collapse waits DIVIDER_FADE_MS so the messages never slide through a visible line.
  const col = logcol();
  if(col && rawUnreadCount(created) === 0 && fadeOutDivider(col)){
    liveGateTimer = setTimeout(collapseAndMerge, DIVIDER_FADE_MS);
    return;
  }
  collapseAndMerge();
}

function abortActive(){
  if(active) api("/api/abort", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: active }) }).catch(()=>{});
}

function sessionContextHint(created, list){
  const s = (list || sessionList).find(x => x.created === created);
  if(!s || !s.ctx_used) return null;
  return { used: s.ctx_used, window: s.ctx_window || 0 };
}

function hasRunningTurn(created){
  return model.turns(created).some(t => t.role === "user" && t.state === "run");
}

async function loadTranscript(created){
  try {
    const r = await api("/api/transcript?session=" + created + "&limit=" + CAP);
    if(!r.ok) return;
    const data = await r.json();
    model.reconcile(created, data.turns || []);
    loaded[created] = true;
    offsetFor[created] = data.offset || 0;
    moreFor[created] = !!data.more;
    // Seed this tab's durable tail cursor from its loaded rows. Each loaded tab has its own cursor,
    // so lazy-loading one session cannot skip content for any other session.
    tailCursors[created] = tailPos(data.turns || []); // where the live tail resumes for this tab
    // Seed the durable read watermark from the server (NOT "all read"): the unread divider then
    // survives reload/restart. Establish it once; later live reads advance it. With content and
    // watermark now known, position the active view — jump to the divider if there is unread.
    if(readThrough[created] === undefined) readThrough[created] = parsePos(data.read_through);
    await ensureLineLoaded(created); // guarantee the unread line + KEEP_ABOVE context are in the window
    if(created === active){
      if(rawUnreadCount(created) > 0){ stick = false; jumpToUnread(created); }
      else { markRead(created); stick = true; }
      renderTabs(active);
    }
    rerenderStructural(created, true);
    // (No explicit capWindow here: positioning above fires a scroll event that re-caps once the DOM
    // is real; capWindow's fits-the-viewport guard needs that real geometry to avoid dropping visible
    // rows on a fresh/short load.)
  } catch(e){}
}

// loadOlder pages in the previous CAP-turn page and PREPENDS it, keeping the viewport stable (the
// scroll position is nudged by the height the prepended content added). Guarded so the scroll-driven
// auto-load and the initial fill can't overlap requests.
// loadOlder pages in the previous CAP-turn page and PREPENDS it. `showTop` (the manual "load earlier"
// button) reveals the just-loaded rows at the top of the viewport; otherwise (scroll-driven auto-load)
// the viewport is kept stable by nudging the scroll by the added height. Guarded against overlap.
async function loadOlder(created, showTop){
  if(!offsetFor[created] || loadingOlder[created]) return; // nothing older, or a load already in flight
  loadingOlder[created] = true;
  const log = document.getElementById("log");
  const oldH = (created === active && log) ? log.scrollHeight : 0;
  try {
    const r = await api("/api/transcript?session=" + created + "&before=" + offsetFor[created] + "&limit=" + CAP);
    if(!r.ok) return;
    const data = await r.json();
    model.prepend(created, data.turns || []);
    offsetFor[created] = data.offset || 0;
    moreFor[created] = !!data.more;
    if(created === active){
      const prev = stick; stick = false; // never snap to the bottom after loading old history
      rerenderStructural(created, true);
      stick = prev;
      if(log){
        if(showTop) log.scrollTop = 0;                   // button: show the older rows just loaded (not off-screen above)
        else log.scrollTop += log.scrollHeight - oldH;   // scroll-driven: keep the current view stable
      }
    }
  } catch(e){} finally { loadingOlder[created] = false; }
}

// rawUnreadCount is the true unread model (line-to-bottom): it drives the in-log divider,
// the jump target, AND the tab badge — so the active tab shows its real remaining count and
// counts down as the reader advances, and badge, title, and divider always agree.
function rawUnreadCount(created){
  const base = readThrough[created];
  if(base === undefined) return 0;
  let n = 0;
  for(const t of model.turns(created)){
    if(t.role !== "user" || t.seq === undefined) continue; // user bubbles + standalone rows don't count
    for(let i = 0; i < (t.blocks || []).length; i++) if(pos(t.seq, i) > base) n++;
  }
  return n;
}
// firstUnreadRow is the index of the "непрочитанные сообщения" line: the first row carrying a block
// after the read watermark. Returns arr.length when everything is read (line at the very bottom).
function firstUnreadRow(created){
  const base = readThrough[created];
  const arr = model.turns(created);
  if(base === undefined) return arr.length;
  for(let i = 0; i < arr.length; i++){
    const t = arr[i];
    if(t.role === "user" && t.seq !== undefined){
      const nb = (t.blocks || []).length;
      if(nb > 0 && pos(t.seq, nb - 1) > base) return i;
    }
  }
  return arr.length;
}
// rowBubbles is a row's on-screen bubble count: a user turn renders as its message bubble PLUS one
// per answer block (assistant text / tool call); a standalone row is one bubble. The window is
// measured in these, so one turn with many tool calls counts as many.
function rowBubbles(t){
  if(t && t.role === "user" && t.seq !== undefined) return 1 + (t.blocks ? t.blocks.length : 0);
  return 1;
}
// bubblesAbove counts bubbles in rows [0, upto).
function bubblesAbove(created, upto){
  const arr = model.turns(created);
  let n = 0;
  for(let i = 0; i < upto && i < arr.length; i++) n += rowBubbles(arr[i]);
  return n;
}
// capWindow trims a session's history from the TOP. It KEEPS everything at/below the unread line (all
// unread) plus a read-context buffer above it, and evicts only older READ rows. For the ACTIVE tab the
// cut is bounded by BOTH: (a) the bubble budget — keep ≥ KEEP_ABOVE bubbles above the line — AND (b)
// the VIEWPORT — a row may be dropped only if its rendered element is ENTIRELY off-screen above the
// viewport (plus a one-screen scrollback buffer). (b) is essential: a tall/zoomed-out viewport can
// show far more than KEEP_ABOVE bubbles, so the bubble budget alone would drop VISIBLE rows and undo a
// manual "load earlier" (contract B4). A background tab has no viewport, so it is bounded by the
// bubble budget once large. Unread/line/divider/badge are never disturbed (evicted rows are read →
// rawUnreadCount unchanged); each evicted row is one transcript page-unit, so offsetFor advances.
// Callers run this only with a CURRENT DOM (post-render / scroll), never on the pre-render model.
function capWindow(created){
  if(!created || readThrough[created] === undefined) return 0; // no watermark yet — cannot prove a row is read
  const arr = model.turns(created);
  if(bubblesAbove(created, arr.length) <= WIN_MAX) return 0; // WHEN: hold up to WIN_MAX bubbles before trimming at all
  const fu = firstUnreadRow(created); // NEVER evict at/after the unread line
  let held = 0, cut = fu; // bubble budget: how many top read rows "keep ≥ KEEP_ABOVE bubbles" allows dropping
  while(cut > 0 && held < KEEP_ABOVE){ cut--; held += rowBubbles(arr[cut]); }
  if(created === active){
    // WHERE (active tab only): additionally bound the cut to rows whose element is ENTIRELY off-screen
    // above the viewport (+1-screen buffer), so a tall/zoomed-out viewport showing > KEEP_ABOVE bubbles
    // never loses VISIBLE rows. The viewport rule narrows WHERE we may cut; it does not change WHEN.
    const log = document.getElementById("log"), col = logcol();
    if(!log || !col) return 0;
    const cutoff = log.getBoundingClientRect().top - log.clientHeight; // a row whose bottom is above this is off-screen
    let vp = 0, ri = 0; // DOM message elements are model rows in order; skip the #more button + the divider
    for(let i = 0; i < col.children.length && ri < cut; i++){
      const el = col.children[i];
      if(el.id === "more" || (el.classList && el.classList.contains("readline"))) continue;
      if(el.getBoundingClientRect().bottom <= cutoff){ ri++; vp = ri; } else break; // first on/near-screen row → stop
    }
    cut = vp; // intersect the bubble budget with the off-screen prefix
  }
  if(cut <= 0) return 0;
  const removed = model.evictTop(created, cut);
  if(removed > 0){
    offsetFor[created] = (offsetFor[created] || 0) + removed;
    moreFor[created] = true;
  }
  return removed;
}
// ensureLineLoaded guarantees the unread line (plus ≥ KEEP_ABOVE bubbles of read context above it) is
// actually in the window after an initial fetch: if the first page landed entirely below the line
// (lots of unread, so the line sits older than the page), pull older pages until the line + its
// context are loaded. Bounded by a guard so a never-read session cannot loop the whole transcript in.
async function ensureLineLoaded(created){
  let guard = 0;
  while(moreFor[created] && bubblesAbove(created, firstUnreadRow(created)) < KEEP_ABOVE && guard++ < 25){
    await loadOlder(created);
  }
}
function advanceReadThroughPastViewport(log){
  if(!active || readThrough[active] === undefined || !log || inReadGrace(active)) return false;
  const top = log.getBoundingClientRect().top;
  let next = readThrough[active];
  log.querySelectorAll("[data-pos]").forEach(el => {
    const p = parseInt(el.dataset.pos || "0", 10) || 0;
    if(p > next && el.getBoundingClientRect().bottom < top + 1) next = p;
  });
  if(next <= readThrough[active]) return false;
  readThrough[active] = next;
  if(rawUnreadCount(active) === 0) delete unreadJump[active];
  else startReadGrace(active);
  reportRead(active);
  return true;
}
// badgeCount is the number a tab shows: the client's precise count for a LOADED tab, or the
// server's unread (from the sessions snapshot) for a tab not yet loaded in this client — so a
// never-opened / background session still shows a badge (finding B).
function badgeCount(created){
  if(loaded[created]) return rawUnreadCount(created);
  const s = sessionList.find(x => x.created === created);
  return (s && s.unread) || 0;
}

async function selectSession(created){
  const switching = active !== created;
  if(active && switching){ rememberScroll(active); saveDraft(active); }
  if(active && switching && documentVisible() && stick){
    markRead(active);
  }
  active = created;
  // The composer travels with the tab: swap in this session's draft before any scroll
  // math below, then re-baseline the resize observer so the swap itself doesn't anchor.
  if(switching){ loadDraft(created); syncComposerH(); }
  if(location.hash !== "#" + created) location.hash = String(created);
  if(!loaded[created]){
    // Not yet loaded: load first (loadTranscript seeds readThrough from the server and then
    // positions the view — jump to the divider if unread, else the bottom).
    await loadTranscript(created);
  } else {
    // Already loaded: returning to unread jumps to the "новые сообщения" divider, else the bottom.
    const hadUnread = rawUnreadCount(created) > 0;
    if(hadUnread) jumpToUnread(created);
    else markRead(created);
    stick = !hadUnread;
    renderTabs(active);
    rerenderStructural(created, true);
    if(!hadUnread) restoreScroll(created);
  }
  focusComposer();
}

// onSessionsList is the SINGLE reconcile path for both /api/sessions and the live
// `sessions` event: it redraws the strip and, if the active session vanished (closed here
// or from another client), drops it and selects a replacement so the tab is never stuck
// on a dead session.
async function onSessionsList(list){
  list = list || [];
  const oldList = sessionList;
  sessionList = list;
  const affected = new Set();
  let activeReadAdvanced = false;
  for(const s of list){
    const oldCtx = sessionContextHint(s.created, oldList);
    const newCtx = sessionContextHint(s.created, list);
    if(loaded[s.created] && hasRunningTurn(s.created) && ((oldCtx && oldCtx.used) !== (newCtx && newCtx.used) || (oldCtx && oldCtx.window) !== (newCtx && newCtx.window))){
      affected.add(s.created);
    }
    // Cross-tab / cross-device read sync: adopt the server's durable read watermark when it is
    // AHEAD of ours — another browser tab (or the messenger) read further. Monotonic (never
    // regresses our own, maybe-not-yet-reported, reading), so the divider + badge here catch up.
    if(loaded[s.created] && s.read_through){
      const p = parsePos(s.read_through);
      if(readThrough[s.created] === undefined || p > readThrough[s.created]){
        readThrough[s.created] = p;
        affected.add(s.created);
        if(s.created === active) activeReadAdvanced = true;
      }
    }
  }
  if(active && !list.some(s => s.created === active)){
    model.drop(active); delete loaded[active]; markRead(active); dropDraft(active);
    active = 0;
  }
  reconcileSessions(list, active);
  // A cross-tab read advance is a DISCRETE change: start the live animation immediately so the
  // marker never lags the badge. commitLive owns the full divider-collapse sequence, including the
  // post-fade merge when the unread line used to split one bubble.
  if(activeReadAdvanced && loaded[active]) commitLive(active);
  else if(affected.has(active) && loaded[active]) scheduleLiveRerender(active);
  if(!active && list.length){
    const want = parseInt(location.hash.slice(1), 10);
    const a = list.find(s => s.created === want) || list.find(s => s.active) || list[0];
    if(a) await selectSession(a.created);
  }
}

async function syncSessions(){
  try {
    const r = await api("/api/sessions");
    if(r.ok) await onSessionsList(await r.json());
  } catch(e){}
}

function showNotice(text){
  const n = document.getElementById("notice");
  if(!n) return;
  n.textContent = text; n.classList.add("show");
  clearTimeout(showNotice._t); showNotice._t = setTimeout(() => n.classList.remove("show"), 5000);
}

// noticeText turns a command-output notice (Telegram HTML) into plain text with line breaks.
function noticeText(s){ return (s || "").replace(/<br\s*\/?>/gi, "\n").replace(/<[^>]+>/g, "").trim(); }

// onNoticeEvent shows the toast AND keeps the notice visible in the active conversation
// (a command/status line stays in the log, not just a 5s toast).
function onNoticeEvent(text){
  const t = noticeText(text);
  showNotice(t);
  if(active){ model.appendStandalone(active, { role: "notice", text: t }); commitLive(active); }
}

// toggleToBottom shows the down-arrow affordance only when the user has scrolled up.
function toggleToBottom(){ const b = document.getElementById("tobottom"); if(b) b.classList.toggle("hidden", stick); }

// fallbackCopy uses the legacy execCommand path for insecure/plain-HTTP origins where
// navigator.clipboard is unavailable.
function fallbackCopy(text, ok){
  try {
    const ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.appendChild(ta); ta.select(); document.execCommand("copy"); document.body.removeChild(ta);
    if(ok) ok();
  } catch(e){}
}
function copyText(text, ok){
  if(navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(ok).catch(() => fallbackCopy(text, ok));
  else fallbackCopy(text, ok);
}
function flashCopied(el){
  if(!el) return;
  el.classList.remove("copyflash");
  void el.offsetWidth;
  el.classList.add("copyflash");
  el.addEventListener("animationend", () => el.classList.remove("copyflash"), { once: true });
}

// setDegraded turns the top-left logo amber while the live channel is down (the poll loop
// is failing and backing off) and restores it on the next good poll — an explicit,
// always-visible "нет соединения" state so a silently frozen UI is never mistaken for idle.
function setDegraded(on){
  const logo = document.querySelector("#bar .logo");
  if(!logo) return;
  logo.classList.toggle("degraded", on);
  logo.setAttribute("aria-label", on ? "klax — нет соединения с сервером" : "klax");
}

// the poll host events.js drives
const host = {
  model,
  ctx: {
    onSessions: list => { onSessionsList(list).catch(e => console.error("klax sessions", e)); },
    onNotice: onNoticeEvent,
  },
  // tailLoop: per-session durable content cursors (loaded tabs only) + the transient-notice cursor.
  cursors: () => { const c = {}; for(const k in loaded){ if(loaded[k] && tailCursors[k]) c[k] = tailCursors[k]; } return c; },
  setTailCursor: (id, cur) => { tailCursors[id] = cur; },
  noticeCursor: () => noticeCursor, setNoticeCursor: c => { noticeCursor = c; },
  sessRev: () => sessRev, setSessRev: v => { sessRev = v; },
  onAffected: set => {
    for(const c of set){
      if(c === active){
        if(documentVisible() && stick){
          markRead(c);
          // NOTE: capWindow is NOT called here — the live render below runs later and the DOM is still
          // pre-update, so a viewport measurement would be stale. The stickToBottom in that render
          // fires a scroll event → the scroll handler re-caps with a CURRENT DOM.
        } else if(rawUnreadCount(c) > 0){
          stick = false;
          startReadGrace(c);
        }
        scheduleLiveRerender(c);
      } else {
        capWindow(c); // background loaded tab: no viewport → bound its model (read rows only) so switching to it is cheap
      }
    }
    renderTabs(active);
  },
  onAuthFail: () => { const a = document.getElementById("app"); if(a) a.classList.remove("active"); const g = document.getElementById("gate"); if(g) g.classList.remove("hidden"); },
  onRestart: () => showNotice("klax обновился"),
  // Show the amber logo only after the 2nd consecutive failure, so a single dropped poll
  // (or a fast daemon restart the next poll rides through) never flashes it; clear on any
  // good poll. The poll loop keeps retrying regardless — this is purely the visible signal.
  onHealth: (ok, fails) => setDegraded(!ok && fails >= 2),
};

async function onNewSession(created){ await syncSessions(); await selectSession(created); }
async function afterClose(created){
  model.drop(created); delete loaded[created]; markRead(created); dropDraft(created);
  if(created === active) active = 0;
  await syncSessions();
}

function start(){
  document.getElementById("gate").classList.add("hidden");
  const app = document.getElementById("app"); if(app) app.classList.add("active");
  initCompose({
    getActive, notice: showNotice,
    isLive: c => sessionList.some(s => s.created === c),
    onAfterSend: () => { stick = true; markRead(active, true); renderTabs(active); stickToBottom(); },
  });
  initTabs({ select: selectSession, onNew: onNewSession, afterClose, notice: showNotice, unread: badgeCount, focus: focusComposer });
  // Delegated copy affordances: the copied object flashes, not the button.
  const lw = document.getElementById("log");
  if(lw) lw.addEventListener("click", e => {
    const target = e.target.closest && e.target.closest(".copy, .mcopy, .body code");
    if(!target) return;
    let text, flash;
    if(target.classList.contains("mcopy")){
      const msg = target.closest(".msg");
      if(msg) text = msg._raw;
      flash = msg;
    } else if(target.classList.contains("copy")){
      const pre = target.closest("pre");
      const code = pre && pre.querySelector("code");
      if(code) text = code.textContent || "";
      flash = pre;
    } else {
      if(target.closest("pre")) return;
      const sel = window.getSelection && window.getSelection();
      if(sel && !sel.isCollapsed) return;
      text = target.textContent || "";
      flash = target;
    }
    if(text === undefined) return;
    copyText(text, () => flashCopied(flash));
  });
  document.addEventListener("selectionchange", () => { if(pendingRender && !selectionInLog(logcol())){ pendingRender = false; commitLive(active); } });
  const log = document.getElementById("log");
  const allowReadOnScroll = () => { readOnScroll = true; };
  if(log){
    log.addEventListener("wheel", allowReadOnScroll, { passive: true });
    log.addEventListener("touchstart", allowReadOnScroll, { passive: true });
  }
  if(log) log.addEventListener("scroll", () => {
    stick = settledDistance(log) < 80; // settled: a FLIP mid-slide must not unstick us
    if(active) scrollTopFor[active] = log.scrollTop;
    // Auto-load older history: only while the user is scrolled UP (not `stick`) and nearing the top of
    // what's loaded. The `!stick` guard is essential: when the whole window fits the viewport (short
    // window / small screen) the view is simultaneously "at the bottom" (stick → capWindow evicts) AND
    // "near the top" (scrollTop small) — without it, auto-load and capWindow ping-pong the same page
    // forever. loadOlder is guarded + preserves the scroll position, so this stays a smooth scroll up.
    if(active && !stick && log.scrollTop < 300 && moreFor[active] && !loadingOlder[active]) loadOlder(active);
    if(readOnScroll && active && documentVisible()){
      const oldTop = log.scrollTop;
      const oldHeight = log.scrollHeight;
      const advanced = advanceReadThroughPastViewport(log);
      if(stick){
        const read = markRead(active);
        const capped = capWindow(active) > 0; // back at the bottom → evict the older rows scrolled up to read (they reload on the next scroll up)
        if(read || advanced || capped){
          renderTabs(active);
          if(capped) rerenderStructural(active); // structural when we evicted: drop the off-screen DOM cleanly
          else commitLive(active); // animate divider collapse and finish any split-bubble merge
        }
      } else if(advanced){
        renderTabs(active);
        rerenderStructural(active);
        log.scrollTop = oldTop + (log.scrollHeight - oldHeight);
        scrollTopFor[active] = log.scrollTop;
      }
    }
    toggleToBottom();
  });
  // Composer is an overlay, not a flex row that shrinks #log. Its height is reserved as a
  // transparent bottom border on #log (so the scrollbar ends at the composer top): pinned
  // re-pins here, a scrolled-up reader's top messages stay put (overflow-anchor:none). Tab
  // switches re-baseline via syncComposerH().
  const composer = document.getElementById("composer");
  if(composer && log && typeof ResizeObserver !== "undefined"){
    setComposerH(composer.offsetHeight);
    new ResizeObserver(() => {
      const d = composer.offsetHeight - composerH;
      setComposerH(composer.offsetHeight);
      if(!d) return;
      if(stick) stickToBottom();
      else toggleToBottom();
    }).observe(composer);
  }
  const th = document.getElementById("theme");
  if(th) th.addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark"));
  const tb = document.getElementById("tobottom");
  if(tb) tb.addEventListener("click", () => { stick = true; markRead(active, true); renderTabs(active); rerenderStructural(active); }); // rerender's stickToBottom fires a scroll event → scroll handler re-caps with a current DOM
  window.addEventListener("hashchange", () => { const w = parseInt(location.hash.slice(1), 10); if(w && w !== active && sessionList.some(s => s.created === w)) selectSession(w); });
  document.addEventListener("keydown", e => {
    if(["ArrowDown","PageDown","End"," "].includes(e.key)) allowReadOnScroll();
  });
  // Read-through advances only while the user can actually see the bottom. Hidden tabs
  // stop advancing; foregrounding an unread active tab performs one jump to the divider.
  document.addEventListener("visibilitychange", () => {
    if(document.visibilityState === "hidden"){
      if(active && stick) markRead(active);
      flushRead(active); // push the read watermark now, before the tab may freeze/close
    } else {
      syncSessions();
      if(active){
        if(rawUnreadCount(active) > 0){
          stick = false;
          jumpToUnread(active);
        } else {
          markRead(active);
        }
        rerenderStructural(active);
      }
    }
  });
  syncSessions().then(() => tailLoop(host)); // durable-tail live channel (POST /api/tail)
}

function gateSubmit(){ const el = document.getElementById("token"); const t = el ? el.value.trim() : ""; if(t){ setToken(t); start(); } }

applyTheme((() => { try { return localStorage.getItem("klax_theme2"); } catch(e){ return null; } })() || "light");
injectEmojiFont();
if(getToken()) start();
else {
  const btn = document.getElementById("tokenbtn"); if(btn) btn.addEventListener("click", gateSubmit);
  const tk = document.getElementById("token"); if(tk) tk.addEventListener("keydown", e => { if(e.key === "Enter") gateSubmit(); });
}
