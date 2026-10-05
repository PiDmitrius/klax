// app.js — bootstrap + wiring. Owns the model, the active-session render flow, and the live-channel
// host (the `after` cursor, window/page loads, event routing); ties compose + tabs together.
// The whole old state machine (runningTurn/doneTurns/queuedTurns/tmpTurn/renderedPending/readMark/
// insertAnswer/breakMerge) is gone — a turn's truth is model turn.state.

import { TurnModel, ordLess, applyMerge } from "./model.js";
import { renderSession, answerBlock, beginShift, playShift, fadeOutDivider, DIVIDER_FADE_MS, pos, parsePos, decodePos } from "./render.js";
import { esc } from "./markdown.js";
import { changesLoop, cursorEpoch, cursorSeq } from "./events.js";
import { api, apiError, hasCoarsePointer, copyText, flashCopied, bindButtonActivation, setHome } from "./base.js";
import { initAuth, isReadOnly } from "./auth.js";
import { selectionInLog } from "./scroll.js";
import { initCompose, updateComposerAccess, saveDraft, loadDraft, dropDraft, recoverOutbox, outboxList } from "./compose.js";
import { initTabs, reconcileSessions, renderTabs } from "./tabs.js";
import { injectEmojiFont } from "./emoji.js";
import { showNotice } from "./notices.js";
import { initSystem, systemRestartNotice } from "./system.js";
import { parseHash, setScope, currentScope, sameScope, inScope, filterScope, storageKey, writeHash, renderChip, closeChipMenu } from "./scope.js";
import { neighborIn } from "./selection.js";
import { initDebug } from "./debug.js";

const model = new TurnModel();
const SERVER_EPOCH_KEY = "klax_server_epoch";
let serverEpoch = null;
try { serverEpoch = sessionStorage.getItem(SERVER_EPOCH_KEY); } catch(_){}
const loaded = {};        // klaxId -> transcript loaded?
const transcriptLoads = {}; // klaxId -> shared initial-load promise
const readThrough = {};   // klaxId -> encoded (turn,block) read watermark (pos()); undefined until seeded
const unreadJump = {};    // klaxId -> one-shot scroll to the unread divider
const readGraceUntil = {}, readGraceTimer = {};
const readReportTimer = {}; // klaxId -> pending POST /api/read debounce timer
const readSaved = {}, readSending = {}; // klaxId -> confirmed / in-flight read position
const READ_GRACE_MS = 1600;
let active = "";
// Live channel. `after` is the ring cursor every applied snapshot/changes response advances. While
// a window or page of a session is in flight its events inside the requested range wait in
// `buffered`; an applied load leaves a skip (its `at` and range) so events it already contains are
// not applied twice.
let after = "", pendingSelect = "";
let tabs = new Map(), tabOrder = []; // the strip as published: tab wire objects by klaxId, and their order
const winReq = {}, loading = {}, buffered = {}, skips = {};
const restoreAfterResync = new Set(); // sessions whose windows a resync still owes
const refreshing = {}; // klaxId -> a window is being replaced in place; the old DOM stays until it is done
let shownSession = ""; // the session the log DOM shows
const affectedNow = new Set();
let bottomJumpFrame = 0;
let stick = true, pendingRender = false, readOnScroll = true;
let readScrollTimer = 0, readTouching = false, readScrollReady = 0;
const SCROLL_IDLE_MS = 160;
let liveRenderRAF = 0, liveRenderSession = "";
// Live DOM commits are serialized so streamed blocks never animate on top of each other.
// While an entrance/FLIP is in flight the model keeps updating, but the DOM commit is
// deferred and COALESCED: everything that arrived during the window then appears as ONE
// block growing out of the dots, instead of a cascade of overlapping slide-ins. Only the
// animation is throttled (COMMIT_MS, a hair over the 180ms entrance) — the data stays live.
const COMMIT_MS = 200;
const MERGE_JOIN_MS = 180;
let liveBusy = false, liveDirty = false, liveGateTimer = 0;
let sessionList = []; // last session strip — for hash-change validity + lookups
let pendingOutboxRecovery = null;
const moreFor = {}; // klaxId -> has-older-history flag (pagination)
const loadingOlder = {}; // klaxId -> a loadOlder() is in flight (guards the auto-load-on-scroll + the initial fill)
// Timeline window (anchored on the "непрочитанные сообщения" line = the read watermark). Measured in
// BUBBLES (a user turn = its message bubble + one per answer block/tool call; a standalone = 1) — the
// unit the user sees, so one big turn counts as many, not one. CAP is the loadOlder page in turn
// groups (server pagination unit).
//   - everything at/below the line (all unread) is ALWAYS kept;
//   - ≥ KEEP_ABOVE bubbles of read context are kept above the line (rounded up to a turn boundary);
//   - older rows evict from the top once total bubbles exceed WIN_MAX (never the viewport-to-bottom range);
//   - the line is guaranteed loaded (ensureLineLoaded pulls older pages if it sits above the first page);
//   - older history auto-loads CAP turns at a time when the user scrolls to the top of what is loaded.
const KEEP_ABOVE = 60, CAP = 20, WIN_MAX = 120;
const scrollTopFor = {}; // klaxId -> last scrollTop, so tab switches do not snap by a pixel
const watchedImages = new WeakSet();
// Assigned when start() wires the observer. A tab switch changes composer height programmatically;
// re-baselining prevents that swap from being mistaken for user-driven textarea/chip growth.
let rebaselineComposerResize = () => {};

function logcol(){ return document.getElementById("logcol"); }
function getActive(){ return active; }

function documentVisible(){ return typeof document === "undefined" || document.visibilityState !== "hidden"; }
function clearReadGrace(klaxId){
  if(!klaxId) return;
  delete readGraceUntil[klaxId];
  if(readGraceTimer[klaxId]){
    clearTimeout(readGraceTimer[klaxId]);
    delete readGraceTimer[klaxId];
  }
}
function inReadGrace(klaxId){ return !!klaxId && (readGraceUntil[klaxId] || 0) > Date.now(); }
function startReadGrace(klaxId){
  if(!klaxId) return;
  readGraceUntil[klaxId] = Date.now() + READ_GRACE_MS;
  if(readGraceTimer[klaxId]) clearTimeout(readGraceTimer[klaxId]);
  readGraceTimer[klaxId] = setTimeout(() => {
    delete readGraceTimer[klaxId];
    if(inReadGrace(klaxId)) return;
    clearReadGrace(klaxId);
    if(active === klaxId && documentVisible() && atBottom() && rawUnreadCount(klaxId) > 0){
      scheduleReadProgress();
    }
  }, READ_GRACE_MS + 40);
}
function markRead(klaxId, force){
  if(!klaxId || !loaded[klaxId]) return false;
  if(!force && inReadGrace(klaxId)) return false;
  const visualChange = rawUnreadCount(klaxId) > 0 || unreadJump[klaxId] !== undefined || readGraceUntil[klaxId] !== undefined;
  const prev = readThrough[klaxId] || 0;
  const next = Math.max(prev, modelMaxPos(klaxId));
  readThrough[klaxId] = next;
  delete unreadJump[klaxId];
  clearReadGrace(klaxId);
  readOnScroll = true;
  if(next !== prev) reportRead(klaxId); // persist ONLY when the watermark actually advanced — no redundant /api/read
  return visualChange;
}
// modelMaxPos is the (turn,block) position of the LAST answer block currently in the model —
// "read up to now". A later block (same turn next index, or a new turn) sorts after it, so a new
// arrival reads as unread. Empty (answerless) turns contribute nothing; their first block, when it
// lands, is unread by its higher turn_seq anyway.
function modelMaxPos(klaxId){
  let max = 0;
  for(const t of model.turns(klaxId)){
    if(t.role === "user" && t.turn_seq !== undefined){
      const bs = t.blocks || [];
      for(let i = bs.length - 1; i >= 0; i--) if(answerBlock(bs[i])){ const p = pos(t.turn_seq, i); if(p > max) max = p; break; }
    }
  }
  return max;
}
// Read reports are debounced; tab-hide flushes pending progress with keepalive before freezing.
// Confirmed positions and positions already in flight need no repeated report.
function reportRead(klaxId){
  if(!klaxId || readThrough[klaxId] === undefined) return;
  if(readReportTimer[klaxId]) return;
  readReportTimer[klaxId] = setTimeout(() => { delete readReportTimer[klaxId]; flushRead(klaxId); }, 400);
}
function flushRead(klaxId){
  if(!klaxId || readThrough[klaxId] === undefined) return;
  if(readReportTimer[klaxId]){ clearTimeout(readReportTimer[klaxId]); delete readReportTimer[klaxId]; }
  const p = readThrough[klaxId];
  if(p <= Math.max(readSaved[klaxId] || 0, readSending[klaxId] || 0)) return;
  readSending[klaxId] = p;
  const { turn, block } = decodePos(p);
  api("/api/read", { method: "POST", keepalive: true, headers: { "Content-Type": "application/json" }, body: JSON.stringify({ klax_id: klaxId, read_pos: turn + "." + block }) })
    .then(r => {
      if(r.ok) readSaved[klaxId] = Math.max(readSaved[klaxId] || 0, p);
      else return apiError(r, "Не удалось сохранить отметку прочитанного").then(showNotice);
    }).catch(()=>{}).finally(() => { if(readSending[klaxId] === p) delete readSending[klaxId]; });
}
function jumpToUnread(klaxId){ if(klaxId){ unreadJump[klaxId] = true; startReadGrace(klaxId); } }
function focusComposer(){
  const input = document.getElementById("input");
  // On a phone, programmatic focus is not equivalent to an open keyboard: depending on whether the
  // call still belongs to a user gesture, Safari may open it, keep a hidden focus, or scroll the
  // textarea under it. Only a real tap focuses the mobile composer. Desktop keeps its keyboard-first
  // workflow and explicit focus restoration.
  if(!input || !documentVisible()) return;
  if(hasCoarsePointer()){
    if(document.activeElement === input) input.blur();
  } else input.focus({ preventScroll: true });
}
function resetMobileComposerFocus(){ if(hasCoarsePointer()) focusComposer(); }
// settledDistance measures how far the view is from the SETTLED bottom of the timeline.
// #logcol.offsetHeight is layout geometry: unlike log.scrollHeight it is NOT inflated by
// the transient FLIP transforms (a unit mid-slide extends the scrollable overflow), so
// stick/pin decisions taken during a 180ms animation stay correct.
function settledDistance(log){
  const col = logcol();
  // The composer is a normal-flow sibling, so log.clientHeight already ends exactly at its top.
  const h = col ? col.offsetHeight : log.scrollHeight;
  return h - log.scrollTop - log.clientHeight;
}
// atBottom is the TRUE "is the settled bottom in view" test, read live from geometry — unlike
// the `stick` flag, which is force-cleared to pin the unread divider on entry and, for a
// conversation that fully fits the viewport, is never recomputed (no scroll event can fire).
// Gating read-advance and the jump button on `stick` then strands a fully-visible session as
// permanently-unread with the button showing; geometry cannot latch that way.
function atBottom(){ const log = document.getElementById("log"); return !log || settledDistance(log) <= 2; }
function stickToBottom(){
  const sc = document.getElementById("log");
  const col = logcol();
  // pin to the settled bottom, not the animation-inflated scrollHeight — pinning to the
  // inflated max overshoots, then snaps back when the slide finishes.
  if(sc) sc.scrollTop = Math.max(0, (col ? col.offsetHeight : sc.scrollHeight) - sc.clientHeight);
  toggleToBottom();
}
function jumpToBottom(){
  releaseBottomJump();
  const log = document.getElementById("log");
  if(!log) return;
  const from = log.scrollTop;
  stick = false;
  markRead(active, true);
  refreshStrip();
  rerenderStructural(active, true);
  const overflow = log.style.overflowY;
  log.style.overflowY = "hidden";
  void log.offsetHeight;
  log.scrollTop = from;
  log.style.overflowY = overflow;
  const start = log.scrollTop;
  let started;
  const reduced = typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;
  const frame = now => {
    if(started === undefined) started = now;
    const progress = reduced ? 1 : Math.min(1, (now - started) / 220);
    const target = Math.max(0, log.scrollTop + settledDistance(log));
    log.scrollTop = start + (target - start) * (1 - Math.pow(1 - progress, 3));
    if(progress < 1){ bottomJumpFrame = requestAnimationFrame(frame); return; }
    bottomJumpFrame = 0;
    stick = true;
    stickToBottom();
    scheduleReadProgress();
    if(liveDirty) scheduleLiveRerender(active);
  };
  bottomJumpFrame = requestAnimationFrame(frame);
}
function releaseBottomJump(){
  if(!bottomJumpFrame) return;
  cancelAnimationFrame(bottomJumpFrame);
  bottomJumpFrame = 0;
  stick = atBottom();
  if(liveDirty) scheduleLiveRerender(active);
}
function rememberScroll(klaxId){
  const log = document.getElementById("log");
  if(klaxId && loaded[klaxId] && log) scrollTopFor[klaxId] = log.scrollTop;
}
function restoreScroll(klaxId){
  const log = document.getElementById("log");
  if(!klaxId || !log || scrollTopFor[klaxId] === undefined) return;
  log.scrollTop = Math.min(scrollTopFor[klaxId], Math.max(0, log.scrollHeight - log.clientHeight));
  stick = atBottom();
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
  // Safari uses theme-color for the browser/status-bar area outside the CSS viewport. Read the same
  // canonical CSS value as #bar instead of maintaining a second set of theme colour literals here.
  const tc = document.getElementById("theme-color");
  if(tc) tc.content = getComputedStyle(document.documentElement).getPropertyValue("--panel").trim();
  const b = document.getElementById("theme"); if(b) b.textContent = t === "dark" ? "☀️" : "🌙";
}

function noMotion(){ return { motionMS: 0, mergeHeldSplits: false, holdSplits: null, stickAfter: false }; }

// rerender(klaxId, live): live=true marks event-driven updates — they run through the
// FLIP snapshot (render.js beginShift/playShift) so new messages slide in and a vanished
// unread divider collapses smoothly instead of jerking the screen. Structural renders
// (tab switch, transcript load, pagination, foregrounding) stay instant — their scroll
// repositioning must not be animated over.
function rerender(klaxId, live, opts){
  opts = opts || {};
  if(klaxId !== active || !loaded[klaxId] || refreshing[klaxId]) return noMotion();
  if(!live && liveBusy && klaxId === active && !opts.forceStructural){
    liveDirty = true;
    return noMotion();
  }
  if(!live){ // a structural render (tab switch, load, foreground) supersedes any queued live animation
    if(liveRenderRAF){ cancelAnimationFrame(liveRenderRAF); liveRenderRAF = 0; liveRenderSession = ""; }
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
  const tab = sessionList.find(s => s.klax_id === active);
  shownSession = active;
  renderSession(col, model.turns(active), readThrough[active], activeReadOnly() ? null : stopTurn, holdSplits, !!opts.joinHeldSplits, tab && tab.ctx_window);
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

function rerenderStructural(klaxId, force){
  return rerender(klaxId, false, { forceStructural: !!force });
}

// scheduleLiveRerender funnels every live content update through the serialization gate.
// Gate OPEN → commit on the next frame (same-frame events still coalesce via the rAF). Gate
// CLOSED (an entrance is playing) → just mark dirty; commitLive's timer flushes the
// accumulated changes as ONE further animation the moment the gate reopens. The model was
// already patched before we got here, so nothing waits on the DOM — only the animation does.
function scheduleLiveRerender(klaxId){
  if(klaxId !== active) return;
  if(liveBusy || bottomJumpFrame){ liveDirty = true; return; } // an animation is in flight — accumulate, don't stack
  liveRenderSession = klaxId;
  if(liveRenderRAF) return;
  liveRenderRAF = requestAnimationFrame(() => {
    const c = liveRenderSession;
    liveRenderRAF = 0;
    liveRenderSession = "";
    if(c === active) commitLive(c);
  });
}

// commitLive paints one animated frame and closes the gate for COMMIT_MS. Whatever arrives
// during that window sets liveDirty and is flushed as a single further animation when the
// gate reopens — so a burst of streamed blocks queues into clean, non-overlapping grows.
function commitLive(klaxId){
  if(liveBusy || bottomJumpFrame){ liveDirty = true; return; } // an animation is in flight — accumulate; openGate flushes it as one further animation
  liveBusy = true;
  liveDirty = false;
  if(liveGateTimer) clearTimeout(liveGateTimer);
  const openGate = () => {
    liveGateTimer = 0;
    const dirty = liveDirty;
    liveBusy = false;
    flushReadProgress();
    if(dirty && active) scheduleLiveRerender(active);
  };
  // Phase 2+: remove the (now-faded) unread line, collapse the gap, then merge any bubble the line split.
  const collapseAndMerge = () => {
    const first = rerender(klaxId, true);
    liveGateTimer = setTimeout(() => {
      if(first.mergeHeldSplits && active === klaxId){
        const joined = rerender(klaxId, true, { holdSplits: first.holdSplits, joinHeldSplits: true });
        const joinWait = Math.max(MERGE_JOIN_MS, joined.motionMS || 0);
        liveGateTimer = setTimeout(() => {
          const merged = rerender(klaxId, true, { noHoldSplits: true });
          if(first.stickAfter && active === klaxId && stick) stickToBottom();
          liveGateTimer = setTimeout(openGate, Math.max(COMMIT_MS, merged.motionMS || 0));
        }, joinWait);
        return;
      }
      if(first.stickAfter && active === klaxId && stick) stickToBottom();
      openGate();
    }, Math.max(COMMIT_MS, first.motionMS || 0));
  };
  // Phase 1 — ONLY when the read line is being dismissed (nothing left unread but the line is still in
  // the DOM): the real in-flow .readline fades out in place where it sits (scrolls with the messages,
  // no ghost). The collapse waits DIVIDER_FADE_MS so the messages never slide through a visible line.
  const col = logcol();
  if(col && rawUnreadCount(klaxId) === 0 && fadeOutDivider(col)){
    liveGateTimer = setTimeout(collapseAndMerge, DIVIDER_FADE_MS);
    return;
  }
  collapseAndMerge();
}

function activeReadOnly(){ return isReadOnly(); }

// stopTurn is the turn ✕: a running turn aborts the whole session (run + queue), a queued one
// drops only itself. Resolves false when the button should become usable again.
function stopTurn(state, seq){
  if(activeReadOnly() || !active) return false;
  const post = (path, body) => api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if(state !== "enq"){ post("/api/abort", { klax_id: active }).catch(()=>{}); return true; }
  return post("/api/cancel", { klax_id: active, turn_seq: seq }).then(r => {
    if(r.ok) return true;
    return r.json().then(b => b.error.message).catch(() => "Не удалось отменить сообщение")
      .then(m => { showNotice(m, "warning"); return false; });
  }, () => { showNotice("Не удалось отменить сообщение", "error"); return false; });
}

function showTranscriptStatus(message = "", retry = false){
  const box = document.getElementById("transcriptstatus");
  box.querySelector("span").textContent = message;
  box.querySelector("button").classList.toggle("hidden", !retry);
  box.classList.toggle("hidden", !message);
  const log = document.getElementById("log");
  log.style.visibility = message ? "hidden" : "";
  log.setAttribute("aria-busy", String(!!message && !retry));
  toggleToBottom();
}

function loadTranscript(klaxId){
  if(!transcriptLoads[klaxId]){
    const p = fetchTranscript(klaxId).finally(() => { if(transcriptLoads[klaxId] === p) delete transcriptLoads[klaxId]; });
    transcriptLoads[klaxId] = p;
  }
  return transcriptLoads[klaxId];
}

// beginLoad/endLoad bracket a window or page request: events of the session inside the requested
// range are buffered meanwhile; an applied load (data) leaves its skip, and once no load is left the
// buffered events are applied in order. A window request supersedes every earlier load of the
// session, so a stale one neither holds the buffer nor touches it when it finishes.
function beginLoad(klaxId, before){
  const e = { before };
  if(before === null){ loading[klaxId] = [e]; delete loadingOlder[klaxId]; }
  else (loading[klaxId] = loading[klaxId] || []).push(e);
  return e;
}
function endLoad(klaxId, e, data){
  const list = loading[klaxId] || [];
  const i = list.indexOf(e);
  if(i < 0) return;
  list.splice(i, 1);
  if(data) (skips[klaxId] = skips[klaxId] || []).push({ at: cursorSeq(data.at), from: data.from, to: data.to });
  if(list.length) return;
  delete loading[klaxId];
  const buf = buffered[klaxId] || [];
  delete buffered[klaxId];
  applySessionEvents(klaxId, buf);
  flushAffected();
}
// fetchJSON bounds a whole request, body included, so a half-open socket cannot wedge a load.
async function fetchJSON(url){
  const ac = new AbortController();
  const t = setTimeout(() => ac.abort(), 30000);
  try {
    const r = await api(url, { signal: ac.signal });
    if(!r.ok) throw new Error(url + " HTTP " + r.status);
    return await r.json();
  } finally { clearTimeout(t); }
}

// fetchRange requests a window or a page before `before`; null when a resync or a server restart
// made it stale, or the session is gone.
async function fetchRange(klaxId, limit, before){
  const data = await fetchJSON("/api/transcript?klax_id=" + encodeURIComponent(klaxId) + (before ? "&to=" + encodeURIComponent(before) : "") + "&limit=" + limit);
  if(cursorEpoch(data.at) !== cursorEpoch(after) || !sessionList.some(s => s.klax_id === klaxId)) return null;
  data.from = data.from ?? null; // absent: the window starts at the history start
  data.to = before || null;
  return data;
}

async function fetchTranscript(klaxId){
  // A newer window request supersedes this one. A session CLOSED while this was in flight was
  // already torn down — repopulating it would leave a dead session with a live model and a `loaded`
  // flag nothing would ever clear. Leaving a group is different — the session still exists, so its
  // state is deliberately kept.
  const req = winReq[klaxId] = (winReq[klaxId] || 0) + 1;
  const e = beginLoad(klaxId, null);
  let data = null;
  try {
    data = await fetchRange(klaxId, CAP, null);
    if(req !== winReq[klaxId]) data = null;
    if(data){
      model.loadWindow(klaxId, data);
      moreFor[klaxId] = !!data.more;
    }
  } catch(err){
    if(klaxId === active && req === winReq[klaxId]) showTranscriptStatus("Не удалось загрузить историю", true);
    return;
  } finally { endLoad(klaxId, e, data); }
  if(!data) return;
  try {
    // Seed the durable read watermark from the server (NOT "all read"): the unread divider then
    // survives reload/restart. Establish it once; later live reads advance it. With content and
    // watermark now known, position the active view — jump to the divider if there is unread.
    if(readThrough[klaxId] === undefined){
      const s = sessionList.find(x => x.klax_id === klaxId);
      readThrough[klaxId] = parsePos(s && s.read_pos);
    }
    await ensureLineLoaded(klaxId); // guarantee the unread line + KEEP_ABOVE context are in the window
    if(!sessionList.some(s => s.klax_id === klaxId) || req !== winReq[klaxId]) return;
    loaded[klaxId] = true;
    if(klaxId === active){
      showTranscriptStatus();
      if(rawUnreadCount(klaxId) > 0){ stick = false; jumpToUnread(klaxId); }
      else { markRead(klaxId); stick = true; }
      refreshStrip();
    }
    rerenderStructural(klaxId, true);
    // (No explicit capWindow here: positioning above fires a scroll event that re-caps once the DOM
    // is real; capWindow's fits-the-viewport guard needs that real geometry to avoid dropping visible
    // rows on a fresh/short load.)
  } catch(err){
    if(klaxId === active) showTranscriptStatus("Не удалось загрузить историю", true);
  }
}

// reloadWindow replaces a session with a fresh window after a delta for a group it lost track of,
// or after a resync.
function reloadWindow(klaxId){
  if(loaded[klaxId] && model.has(klaxId)){ refreshWindow(klaxId); return; }
  delete transcriptLoads[klaxId];
  loadTranscript(klaxId);
}

// refreshWindow replaces a held session's window in place: the new window and the history the
// session held before are loaded aside while its events wait in the buffer, then swapped in and
// rendered once with the visible message kept where it was. A failed load keeps the old view and
// retries with a growing pause.
async function refreshWindow(klaxId, attempt = 0){
  const req = winReq[klaxId] = (winReq[klaxId] || 0) + 1;
  const held = model.has(klaxId), cover = model.rangeStart(klaxId);
  refreshing[klaxId] = req;
  const e = beginLoad(klaxId, null);
  const aside = new TurnModel(), loads = [];
  let more = false;
  try {
    const w = await fetchRange(klaxId, CAP, null);
    if(!w) throw new Error("stale window");
    aside.loadWindow(klaxId, w);
    loads.push(w);
    more = !!w.more;
    while(held && more && ordLess(cover, aside.rangeStart(klaxId))){
      const page = await fetchRange(klaxId, CAP, aside.rangeStart(klaxId));
      if(!page) throw new Error("stale page");
      aside.loadPage(klaxId, page);
      loads.push(page);
      more = !!page.more;
    }
  } catch(err){
    if(req !== winReq[klaxId]) return;
    if(attempt === 2) showNotice("Не удалось обновить историю сессии — повторяю", "warning");
    setTimeout(() => { if(winReq[klaxId] === req && refreshing[klaxId] === req) refreshWindow(klaxId, attempt + 1); }, 1000 << Math.min(attempt, 5));
    return;
  }
  if(req !== winReq[klaxId]) return;
  model.bySession[klaxId] = aside.bySession[klaxId];
  moreFor[klaxId] = more;
  for(const l of loads.slice(1)) (skips[klaxId] = skips[klaxId] || []).push({ at: cursorSeq(l.at), from: l.from, to: l.to });
  endLoad(klaxId, e, loads[0]);
  try { await ensureLineLoaded(klaxId); } catch(err){}
  if(refreshing[klaxId] !== req) return;
  delete refreshing[klaxId];
  const anchor = klaxId === active && shownSession === klaxId && !stick ? viewAnchor() : null;
  if(klaxId === active) showTranscriptStatus();
  rerenderStructural(klaxId, true);
  if(anchor) restoreAnchor(anchor);
}

// viewAnchor records the first message visible in the log and its offset from the log's top;
// restoreAnchor scrolls so that message is back at the same offset.
function viewAnchor(){
  const log = document.getElementById("log"), col = logcol();
  if(!log || !col) return null;
  const top = log.getBoundingClientRect().top;
  for(const el of col.children){
    if(el.dataset && /^turn:/.test(el.dataset.renderKey || "") && el.getBoundingClientRect().bottom > top) return { key: el.dataset.renderKey, offset: el.getBoundingClientRect().top - top };
  }
  return null;
}
function restoreAnchor(a){
  const log = document.getElementById("log"), col = logcol();
  if(!log || !col) return;
  for(const el of col.children){
    if(el.dataset && el.dataset.renderKey === a.key){
      log.scrollTop += el.getBoundingClientRect().top - log.getBoundingClientRect().top - a.offset;
      return;
    }
  }
}

// loadOlder pages in the previous CAP-group page and merges it. `showTop` (the manual "load earlier"
// button) reveals the just-loaded rows at the top of the viewport; otherwise (scroll-driven auto-load)
// the viewport is kept stable by nudging the scroll by the added height. Guarded against overlap.
async function loadOlder(klaxId, showTop){
  // Nothing older, a page already in flight, or a window on its way that will replace the range.
  if(!moreFor[klaxId] || loadingOlder[klaxId] || !model.has(klaxId) || (loading[klaxId] || []).some(l => l.before === null)) return;
  const token = loadingOlder[klaxId] = {};
  const log = document.getElementById("log");
  const oldH = (klaxId === active && log) ? log.scrollHeight : 0;
  const req = winReq[klaxId], before = model.rangeStart(klaxId);
  const e = beginLoad(klaxId, before);
  let data = null;
  try {
    data = await fetchRange(klaxId, CAP, before);
    if(req !== winReq[klaxId] || !model.has(klaxId) || model.rangeStart(klaxId) !== before) data = null;
    if(!data) return;
    model.loadPage(klaxId, data);
    moreFor[klaxId] = !!data.more;
    if(klaxId === active && loaded[klaxId] && !refreshing[klaxId]){
      const prev = stick; stick = false; // never snap to the bottom after loading old history
      rerenderStructural(klaxId, true);
      stick = prev;
      if(log){
        if(showTop) log.scrollTop = 0;                   // button: show the older rows just loaded (not off-screen above)
        else log.scrollTop += log.scrollHeight - oldH;   // scroll-driven: keep the current view stable
      }
    }
  } catch(err){
    if(!loaded[klaxId]) throw err;
  } finally {
    endLoad(klaxId, e, data);
    if(loadingOlder[klaxId] === token) delete loadingOlder[klaxId];
  }
}

// rawUnreadCount is the true unread model (line-to-bottom): it drives the in-log divider,
// the jump target, AND the tab badge — so the active tab shows its real remaining count and
// counts down as the reader advances, and badge, title, and divider always agree.
function rawUnreadCount(klaxId){
  const base = readThrough[klaxId];
  if(base === undefined) return 0;
  let n = 0;
  for(const t of model.turns(klaxId)){
    if(t.role !== "user" || t.turn_seq === undefined) continue; // user bubbles + standalone rows don't count
    for(let i = 0; i < (t.blocks || []).length; i++) if(pos(t.turn_seq, i) > base && answerBlock(t.blocks[i])) n++;
  }
  return n;
}
// firstUnreadRow is the index of the "непрочитанные сообщения" line: the first row carrying a block
// after the read watermark. Returns arr.length when everything is read (line at the very bottom).
function firstUnreadRow(klaxId){
  const base = readThrough[klaxId];
  const arr = model.turns(klaxId);
  if(base === undefined) return arr.length;
  for(let i = 0; i < arr.length; i++){
    const t = arr[i];
    if(t.role === "user" && t.turn_seq !== undefined){
      if((t.blocks || []).some((b, bi) => pos(t.turn_seq, bi) > base && answerBlock(b))) return i;
    }
  }
  return arr.length;
}
// rowBubbles is a row's on-screen bubble count: a user turn renders as its message bubble PLUS one
// per answer block (assistant text / tool call); a standalone row is one bubble. The window is
// measured in these, so one turn with many tool calls counts as many.
function rowBubbles(t){
  if(t && t.role === "user" && t.turn_seq !== undefined) return 1 + (t.blocks ? t.blocks.length : 0);
  return 1;
}
// bubblesAbove counts bubbles in rows [0, upto).
function bubblesAbove(klaxId, upto){
  const arr = model.turns(klaxId);
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
// rawUnreadCount unchanged); whole turn groups are evicted and the held range starts after them.
// Callers run this only with a CURRENT DOM (post-render / scroll), never on the pre-render model.
function capWindow(klaxId){
  if(!klaxId || readThrough[klaxId] === undefined || refreshing[klaxId]) return 0; // no watermark yet — cannot prove a row is read
  const arr = model.turns(klaxId);
  if(bubblesAbove(klaxId, arr.length) <= WIN_MAX) return 0; // WHEN: hold up to WIN_MAX bubbles before trimming at all
  const fu = firstUnreadRow(klaxId); // NEVER evict at/after the unread line
  let held = 0, cut = fu; // bubble budget: how many top read rows "keep ≥ KEEP_ABOVE bubbles" allows dropping
  while(cut > 0 && held < KEEP_ABOVE){ cut--; held += rowBubbles(arr[cut]); }
  if(klaxId === active){
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
  const removed = model.evictTop(klaxId, cut);
  if(removed > 0) moreFor[klaxId] = true;
  return removed;
}
// ensureLineLoaded guarantees the unread line (plus ≥ KEEP_ABOVE bubbles of read context above it) is
// actually in the window after an initial fetch: if the first page landed entirely below the line
// (lots of unread, so the line sits older than the page), pull older pages until the line + its
// context are loaded. Bounded by a guard so a never-read session cannot loop the whole transcript in.
async function ensureLineLoaded(klaxId){
  let guard = 0;
  while(sessionList.some(s => s.klax_id === klaxId) && moreFor[klaxId] && bubblesAbove(klaxId, firstUnreadRow(klaxId)) < KEEP_ABOVE && guard++ < 25){
    await loadOlder(klaxId);
  }
}
function resetReadScroll(){
  releaseBottomJump();
  clearTimeout(readScrollTimer);
  readScrollTimer = 0; readScrollReady = 0; readTouching = false;
}

function initReadTouch(log){
  log.addEventListener("touchstart", () => {
    resetReadScroll();
    readOnScroll = true;
    readTouching = true;
  }, { passive: true });
  const releaseTouch = () => { readTouching = false; scheduleReadProgress(); };
  document.addEventListener("touchend", e => { if(readTouching && !e.touches.length) releaseTouch(); }, { passive: true });
  document.addEventListener("touchcancel", () => { if(readTouching) releaseTouch(); }, { passive: true });
}

// Read-driven DOM changes wait for a quiet scroll and a released touch, and never overlap live motion.
function scheduleReadProgress(){
  clearTimeout(readScrollTimer);
  readScrollReady = 0;
  const klaxId = active;
  readScrollTimer = setTimeout(() => {
    readScrollTimer = 0;
    if(klaxId !== active || readTouching || !loaded[klaxId] || !documentVisible()) return;
    readScrollReady = klaxId;
    flushReadProgress();
  }, SCROLL_IDLE_MS);
}

function flushReadProgress(){
  if(liveBusy || bottomJumpFrame || !readScrollReady) return;
  const klaxId = readScrollReady;
  readScrollReady = 0;
  if(klaxId !== active || readTouching || !loaded[klaxId] || !documentVisible()) return;
  const log = document.getElementById("log");
  if(log) settleReadProgress(log);
}

function settleReadProgress(log){
  const bottom = atBottom();
  if((readOnScroll || bottom) && active && documentVisible()){
    const oldTop = log.scrollTop;
    const oldHeight = log.scrollHeight;
    const advanced = !bottom && advanceReadThroughPastViewport(log);
    if(bottom){
      const read = markRead(active);
      const capped = capWindow(active) > 0;
      if(read || capped){
        refreshStrip();
        if(capped) rerenderStructural(active);
        else commitLive(active);
      }
    } else if(advanced){
      refreshStrip();
      rerenderStructural(active);
      log.scrollTop = oldTop + (log.scrollHeight - oldHeight);
      scrollTopFor[active] = log.scrollTop;
    }
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
function badgeCount(klaxId){
  if(loaded[klaxId]) return rawUnreadCount(klaxId);
  const s = sessionList.find(x => x.klax_id === klaxId);
  return (s && s.unread) || 0;
}

async function selectSession(klaxId){
  const switching = active !== klaxId;
  if(active && switching){ rememberScroll(active); saveDraft(active); }
  if(active && switching && documentVisible() && stick){
    markRead(active, true);
  }
  if(switching) resetReadScroll();
  active = klaxId;
  if(switching && selectionInLog(logcol())) window.getSelection().removeAllRanges();
  // The composer travels with the tab. Its draft swap is programmatic session state, not a reason to
  // alter this session's scroll intent, so exclude that height change from composer resize anchoring.
  if(switching){ loadDraft(klaxId); rebaselineComposerResize(); }
  writeHash(klaxId);
  // Persist the viewed tab per-browser AND per-scope so a FRESH open (no URL hash — bookmark, new
  // tab, base URL) restores it instead of falling back to the first tab, and so a root window and a
  // group window don't fight over one remembered tab. Cheap; survives reloads and restarts.
  try { localStorage.setItem(storageKey(), klaxId); } catch(e){}
  refreshStrip();
  focusComposer();
  if(!loaded[klaxId]){
    stick = false;
    showTranscriptStatus("Загрузка истории…");
    // Not yet loaded: load first (loadTranscript seeds readThrough from the server and then
    // positions the view — jump to the divider if unread, else the bottom).
    await loadTranscript(klaxId);
  } else {
    // While its window is refreshed in place the log still shows another session: keep it hidden.
    showTranscriptStatus(refreshing[klaxId] && shownSession !== klaxId ? "Загрузка истории…" : "");
    // Already loaded: returning to unread jumps to the "новые сообщения" divider, else the bottom.
    const hadUnread = rawUnreadCount(klaxId) > 0;
    if(hadUnread) jumpToUnread(klaxId);
    else markRead(klaxId);
    stick = !hadUnread;
    refreshStrip();
    rerenderStructural(klaxId, true);
    if(!hadUnread) restoreScroll(klaxId);
  }
}

// onSessionsList is the SINGLE reconcile path for both the snapshot and the live `sessions` event:
// it redraws the strip and, if the active session left this window (closed anywhere, or dropped out
// of the current group), picks a replacement so the tab is never stuck on a session it cannot show.
// It NEVER awaits: `selectSession` assigns the active session synchronously and only awaits the
// transcript fetch, which nothing here depends on. That keeps the whole reconcile one uninterrupted
// transaction — a generation counter would not have been enough, because the loop below mutates the
// shared read watermark BEFORE any await, so a stale invocation bailing out afterwards would leave
// that advance applied but never animated (the next invocation sees the watermark already raised and
// no longer treats it as a change).
async function onSessionsList(list){
  list = list || [];
  const oldList = sessionList;
  sessionList = list;
  // Restore any submitted-but-unconfirmed messages (durable outbox) BEFORE the first tab is selected,
  // so the active tab's recovered text loads straight into the composer via selectSession→loadDraft.
  // Runs once, as soon as we know the session list.
  if(!isReadOnly() && pendingOutboxRecovery !== null && list.length){
    recoverOutbox({ isLive: c => list.some(s => s.klax_id === c), notice: showNotice }, pendingOutboxRecovery);
    pendingOutboxRecovery = null;
  }
  const affected = new Set();
  let activeReadAdvanced = false;
  for(const c of Object.keys(loaded).concat(Object.keys(loading))){
    if(!list.some(s => s.klax_id === c)) forgetSession(c);
  }
  for(const s of list){
    // Cross-tab / cross-device read sync: adopt the server's durable read watermark when it is
    // AHEAD of ours — another browser tab (or the messenger) read further. Monotonic (never
    // regresses our own, maybe-not-yet-reported, reading), so the divider + badge here catch up.
    // A watermark kept across a resync is raised too, before its window reloads.
    const p = parsePos(s.read_pos);
    readSaved[s.klax_id] = Math.max(readSaved[s.klax_id] || 0, p);
    if(readThrough[s.klax_id] !== undefined){
      if(p > readThrough[s.klax_id]){
        readThrough[s.klax_id] = p;
        if(loaded[s.klax_id]){
          affected.add(s.klax_id);
          if(s.klax_id === active) activeReadAdvanced = true;
        }
      }
    }
  }
  const visible = filterScope(list);
  if(active && !visible.some(s => s.klax_id === active)){
    // The viewed session left THIS window's scope. Two different events land here and they are not
    // the same loss: a session closed anywhere is gone for good (tear its state down), while one
    // that merely lost the group still exists and another window may be working in it — so keep its
    // model and, above all, its composer draft. Either way focus its neighbour in the OLD visible
    // order: the same rule as closing a tab here, now shared by every way a tab can leave.
    const gone = !list.some(s => s.klax_id === active);
    const next = neighborIn(filterScope(oldList), active, visible);
    if(gone) dropActive();
    else leaveActive(); // still exists elsewhere: bank what is in the composer before letting go
    if(next) selectLater(next); // not awaited: `active` is set synchronously, the transcript follows
  }
  if(pendingSelect && list.some(s => s.klax_id === pendingSelect)){
    const c = pendingSelect;
    pendingSelect = "";
    if(c !== active) selectLater(c);
  }
  if(!active && visible.length){
    // Restore priority: explicit URL hash → this scope's last-viewed tab (localStorage) →
    // the server's active flag → first tab. Both remembered ids fall through if that session
    // was since closed (find returns undefined), so a stale value can never strand the UI.
    // This runs BEFORE the strip is drawn: selection assigns `active` synchronously and only the
    // transcript is fetched afterwards, so drawing first would paint a strip with no active tab and
    // leave it that way until the fetch returned — or forever, if it never did.
    let stored = "";
    try { stored = localStorage.getItem(storageKey()) || ""; } catch(e){}
    const want = parseHash().klax_id || stored;
    const a = visible.find(s => s.klax_id === want) || visible.find(s => s.active) || visible[0];
    if(a) selectLater(a.klax_id);
  }
  reconcileSessions(visible, active);
  renderChip(list, badgeCount);
  // A cross-tab read advance is a DISCRETE change: start the live animation immediately so the
  // marker never lags the badge. commitLive owns the full divider-collapse sequence, including the
  // post-fade merge when the unread line used to split one bubble.
  if(activeReadAdvanced && loaded[active]) commitLive(active);
  else if(affected.has(active) && loaded[active]) scheduleLiveRerender(active);
  setEmptyScope(!visible.length);
}

// selectLater starts a selection without making the caller wait for the transcript. The selection
// itself (which tab is active, its draft, the address bar) happens synchronously inside; only the
// fetch is left running, and its own render path is guarded on the session still being active.
function selectLater(klaxId){
  // Transcript and API failures are already absorbed inside loadTranscript, so anything surfacing
  // here is a programming or DOM error — swallowing it silently would hide a real bug.
  selectSession(klaxId).catch(e => console.error("klax: select session", klaxId, e));
}

// leaveActive / dropActive are the TWO ways this window stops showing the active session, and the
// difference between them is what survives. A session that merely left the current scope still
// exists — another window may be typing in it — so its scroll position and, above all, the text
// sitting in the composer are banked first; `selectSession` cannot do it for us, since it only saves
// while `active` is still set. A session that is genuinely gone is torn down instead.
function leaveActive(){
  if(!active) return;
  rememberScroll(active); saveDraft(active);
  active = "";
}
function dropActive(){
  if(!active) return;
  markRead(active); dropDraft(active);
  active = "";
}
// forgetSession drops a gone session's model, loads and pending events; a load or retry still in
// flight sees its request superseded and stops.
function forgetSession(c){
  winReq[c] = (winReq[c] || 0) + 1;
  model.drop(c);
  delete loaded[c]; delete loading[c]; delete refreshing[c]; delete buffered[c]; delete skips[c];
}

// setEmptyScope: an empty group view stays put instead of teleporting to root, which would be
// surprising; an unknown group in the URL is just an empty view, not an error. It only
// TOGGLES visibility — the log DOM is left intact so returning to a populated scope re-renders from
// the model instead of rebuilding from scratch.
function setEmptyScope(on){
  document.body.classList.toggle("emptyscope", !!on);
  const box = document.getElementById("scopeempty");
  if(!box) return;
  const name = currentScope().name;
  box.innerHTML = !on ? "" :
    (currentScope().kind === "builtin"
      ? 'Вид «' + esc(name) + '» ещё не реализован.'
      : 'В группе «' + esc(name) + '» нет сессий.');
}

// bootState loads the session snapshot and samples system status.
async function bootState(){
  const data = await fetchJSON("/api/state");
  const epoch = cursorEpoch(data.at);
  const system = data.system;
  if(serverEpoch !== null && epoch !== serverEpoch && system.uptime_sec < 300) showNotice(systemRestartNotice(system.startup, system.version));
  serverEpoch = epoch;
  try { sessionStorage.setItem(SERVER_EPOCH_KEY, epoch); } catch(_){}
  setHome(system.home);
  after = data.at;
  tabs = new Map((data.sessions || []).map(t => [t.klax_id, t]));
  tabOrder = (data.sessions || []).map(t => t.klax_id);
  await onSessionsList(data.sessions || []);
}

// resync reloads the strip from a snapshot, refreshes held sessions in place (keeping the view) and
// loads the ones that were loading. Startup is a resync with nothing held.
async function resync(){
  for(const c of Object.keys(loaded).filter(k => loaded[k]).concat(Object.keys(loading))) restoreAfterResync.add(c);
  if(active) restoreAfterResync.add(active);
  for(const c of restoreAfterResync) winReq[c] = (winReq[c] || 0) + 1;
  for(const m of [loading, buffered, skips, transcriptLoads, loadingOlder, refreshing]) for(const k of Object.keys(m)) delete m[k];
  after = "";
  await bootState();
  const keep = [...restoreAfterResync];
  restoreAfterResync.clear();
  for(const c of keep){
    if(!sessionList.some(s => s.klax_id === c)) continue;
    if(loaded[c] && model.has(c)) refreshWindow(c);
    else if(c === active) selectLater(c);
    else loadTranscript(c);
  }
}

// applyEvents applies one changes response in ring order and advances the cursor. Tab patches and
// orders rebuild the strip, which is reconciled once per response.
function applyEvents(events, at){
  let strip = false;
  for(const ev of events){
    if(ev.notice !== undefined) onNoticeEvent(ev.notice);
    else if(ev.tab){ tabs.set(ev.klax_id, applyMerge(tabs.get(ev.klax_id) || { klax_id: ev.klax_id }, ev.tab)); strip = true; }
    else if(ev.tabs){ tabOrder = ev.tabs; strip = true; }
    else if(ev.klax_id) routeSessionEvent(ev);
  }
  if(strip){
    for(const c of [...tabs.keys()]) if(!tabOrder.includes(c)) tabs.delete(c);
    onSessionsList(tabOrder.map(c => tabs.get(c)).filter(Boolean)).catch(e => console.error("klax sessions", e));
  }
  after = at;
  const seq = cursorSeq(at);
  for(const c of Object.keys(skips)){
    if(loading[c]) continue; // buffered events still need the skips
    skips[c] = skips[c].filter(s => s.at > seq);
    if(!skips[c].length) delete skips[c];
  }
  flushAffected();
}

function eventOrd(ev){ return ev.group ? ev.group.ord : ev.removed ? ev.removed.ord : null; }

function routeSessionEvent(ev){
  const c = ev.klax_id;
  if(loading[c] && loading[c].some(l => !l.before || ordLess(eventOrd(ev), l.before))){
    (buffered[c] = buffered[c] || []).push(ev);
    return;
  }
  applySessionEvents(c, [ev]);
}

function applySessionEvents(klaxId, evs){
  for(const ev of evs){
    if(!model.has(klaxId)) continue;
    const o = eventOrd(ev);
    if((skips[klaxId] || []).some(s => ev.seq <= s.at && !ordLess(o, s.from) && (!s.to || ordLess(o, s.to)))) continue;
    if(ev.removed) model.applyRemoved(klaxId, ev.removed.key);
    else if(!model.applyGroup(klaxId, ev.group)){ reloadWindow(klaxId); return; }
    affectedNow.add(klaxId);
  }
}

function flushAffected(){
  if(!affectedNow.size) return;
  const set = new Set(affectedNow);
  affectedNow.clear();
  host.onAffected(set);
}

// noticeText turns a command-output notice (Telegram HTML) into plain text with line breaks.
function noticeText(s){ return (s || "").replace(/<br\s*\/?>/gi, "\n").replace(/<[^>]+>/g, "").trim(); }

// System messages have one UI surface: the transient bottom-up notification stack.
// They never enter the session model/timeline (which made them appear and then vanish on reload).
function onNoticeEvent(text){
  const t = noticeText(text);
  showNotice(t);
}

// toggleToBottom shows the down-arrow affordance only when the user has scrolled up.
function toggleToBottom(){ const b = document.getElementById("tobottom"); if(b) b.classList.toggle("hidden", !loaded[active] || atBottom()); }

// setDegraded turns the top-left logo amber while the live channel is down (the poll loop
// is failing and backing off) and restores it on the next good poll — an explicit,
// always-visible "нет соединения" state so a silently frozen UI is never mistaken for idle.
function setDegraded(on){
  const logo = document.querySelector("#bar .logo");
  if(!logo) return;
  logo.classList.toggle("degraded", on);
  const button = document.getElementById("sysbtn");
  const label = on ? "klax — нет соединения с сервером" : "klax — состояние системы";
  if(button){ button.setAttribute("aria-label", label); button.title = label; }
}

// the poll host events.js drives
const host = {
  after: () => after,
  apply: applyEvents,
  resync,
  onAffected: set => {
    for(const c of set){
      if(!loaded[c]) continue;
      if(c === active){
        if(documentVisible() && (stick || bottomJumpFrame)){
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
    refreshStrip();
  },
  // Show the amber logo only after the 2nd consecutive failure, so a single dropped poll
  // (or a fast daemon restart the next poll rides through) never flashes it; clear on any
  // good poll. The poll loop keeps retrying regardless — this is purely the visible signal.
  onHealth: (ok, fails) => setDegraded(!ok && fails >= 2),
};

// refreshStrip repaints BOTH surfaces that display unread counts — the tab badges and the scope
// chip (its outside-this-group counter and the menu's per-group numbers) — from the same
// client-side count, in one call. Repainting only the strip made the two disagree until the next
// server broadcast: badges dropped the moment you read, the chip kept the stale number.
function refreshStrip(){ updateComposerAccess(activeReadOnly()); renderTabs(active); renderChip(sessionList, badgeCount); }

// A new session is selected once the strip carries it.
async function onNewSession(klaxId){
  if(sessionList.some(s => s.klax_id === klaxId)) await selectSession(klaxId);
  else pendingSelect = klaxId;
}
// The neighbour rule itself lives in selection.js so it can be tested without the UI; here it is
// only ever applied to the CURRENT scope's order.
function neighborKlaxId(closed){ return neighborIn(filterScope(sessionList), closed); }
async function afterClose(klaxId){
  // Closing the ACTIVE tab focuses its neighbor (left, else right) — not a jump to the first tab.
  // Closing a background tab never moves focus. Compute the neighbor while `closed` is still in the
  // strip order, and select it before the strip update so onSessionsList keeps it.
  const wasActive = klaxId === active;
  const next = wasActive ? neighborKlaxId(klaxId) : "";
  forgetSession(klaxId); markRead(klaxId); dropDraft(klaxId, true);
  if(wasActive){
    active = "";
    if(next) await selectSession(next);
  }
}

function start(){
  pendingOutboxRecovery = outboxList();
  document.getElementById("newtab").classList.toggle("hidden", isReadOnly());
  updateComposerAccess(isReadOnly());
  setScope(parseHash().scope); // the address bar decides the scope before the first strip render
  document.getElementById("gate").classList.add("hidden");
  const app = document.getElementById("app"); if(app) app.classList.add("active");
  initSystem({ notice: showNotice });
  initDebug({ notice: showNotice });
  initCompose({
    getActive, readOnly: activeReadOnly, notice: showNotice,
    onAfterSend: () => { releaseBottomJump(); stick = true; markRead(active, true); refreshStrip(); stickToBottom(); },
  });
  initTabs({ select: selectSession, onNew: onNewSession, afterClose, notice: showNotice, unread: badgeCount,
             focus: focusComposer, allSessions: () => sessionList });
  // Delegated copy affordances: the copied object flashes, not the button.
  const lw = document.getElementById("log");
  if(lw) lw.addEventListener("click", e => {
    const target = e.target.closest && e.target.closest(".copy, .mcopy, .body code");
    if(target){
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
      return;
    }
    const msg = e.target.closest && e.target.closest(".msg.has-actions");
    const sel = window.getSelection && window.getSelection();
    const interactive = e.target.closest && e.target.closest("a, button, input, textarea, select, label");
    if(!msg || interactive || (sel && !sel.isCollapsed)) return;
    const show = !msg.classList.contains("show-actions");
    lw.querySelectorAll(".msg.show-actions").forEach(el => el.classList.remove("show-actions"));
    if(show) msg.classList.add("show-actions");
  });
  document.addEventListener("click", e => {
    if(e.target.closest && e.target.closest("#log .msg.has-actions")) return;
    document.querySelectorAll("#log .msg.show-actions").forEach(el => el.classList.remove("show-actions"));
  });
  document.addEventListener("selectionchange", () => { if(pendingRender && !selectionInLog(logcol())){ pendingRender = false; commitLive(active); } });
  const log = document.getElementById("log");
  const allowReadOnScroll = () => { readOnScroll = true; };
  if(log){
    log.addEventListener("wheel", () => { releaseBottomJump(); allowReadOnScroll(); }, { passive: true });
    log.addEventListener("pointerdown", releaseBottomJump, { passive: true });
    initReadTouch(log);
  }
  document.addEventListener("keydown", e => {
    if(e.defaultPrevented || e.target.closest("input, textarea, select, [contenteditable]")) return;
    if(["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(e.key)) releaseBottomJump();
  });
  if(log) log.addEventListener("scroll", () => {
    if(!loaded[active]) return;
    if(bottomJumpFrame) return;
    stick = atBottom();
    if(active) scrollTopFor[active] = log.scrollTop;
    // Auto-load older history: only while the user is scrolled UP (not `stick`) and nearing the top of
    // what's loaded. The `!stick` guard is essential: when the whole window fits the viewport (short
    // window / small screen) the view is simultaneously "at the bottom" (stick → capWindow evicts) AND
    // "near the top" (scrollTop small) — without it, auto-load and capWindow ping-pong the same page
    // forever. loadOlder is guarded + preserves the scroll position, so this stays a smooth scroll up.
    if(active && !stick && log.scrollTop < 300 && moreFor[active] && !loadingOlder[active]) loadOlder(active);
    scheduleReadProgress();
    toggleToBottom();
  });
  // Composer is a normal-flow flex sibling, so layout itself reserves its exact height in the same
  // frame. The observer only enforces the canonical `stick` decision; it must never turn sticking on
  // itself, because unread navigation and history restoration deliberately turn it off.
  const composer = document.getElementById("composer");
  if(composer && log && typeof ResizeObserver !== "undefined"){
    let lastH = composer.offsetHeight;
    rebaselineComposerResize = () => { lastH = composer.offsetHeight; };
    new ResizeObserver(() => {
      const h = composer.offsetHeight;
      if(h === lastH) return;
      lastH = h;
      if(stick) stickToBottom();
      else toggleToBottom();
    }).observe(composer, { box: "border-box" });
  }
  const th = document.getElementById("theme");
  document.querySelector("#transcriptstatus button").addEventListener("click", () => { if(active) selectLater(active); });
  if(th) th.addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark"));
  const tb = document.getElementById("tobottom");
  if(tb) bindButtonActivation(tb, jumpToBottom);
  // The hash is the window's address: a changed SCOPE is real navigation (re-filter the strip and
  // re-pick a tab), a changed tab within the same scope is just a selection.
  window.addEventListener("hashchange", async () => {
    const p = parseHash();
    if(!sameScope(p.scope, currentScope())){
      closeChipMenu();
      setScope(p.scope);
      // An explicit tab in the address outranks everything: `#work/<klax_id>` means THAT tab, even
      // when the session we are already on also belongs to the new scope. Otherwise keep the current
      // session if the new scope holds it (root always does), so moving between a group and root
      // does not lose your place; failing both, the reconcile picks this scope's remembered tab.
      const explicit = !!p.klax_id && sessionList.some(s => s.klax_id === p.klax_id && inScope(s));
      const keep = !explicit && !!active && sessionList.some(s => s.klax_id === active && inScope(s));
      if(!keep) leaveActive();
      await onSessionsList(sessionList);
      if(keep) writeHash(active);
      return;
    }
    // Same scope, no tab named: the menu's root link is a bare `#`, so landing here from root means
    // the address stopped identifying the viewed tab. Put it back rather than leave a URL that no
    // longer points at what is on screen.
    if(!p.klax_id){ writeHash(active); return; }
    if(p.klax_id !== active && sessionList.some(s => s.klax_id === p.klax_id && inScope(s))) selectSession(p.klax_id);
  });
  document.addEventListener("keydown", e => {
    if(["ArrowDown","PageDown","End"," "].includes(e.key)) allowReadOnScroll();
  });
  // Read-through advances only while the user can actually see the bottom. Hidden tabs
  // stop advancing; foregrounding an unread active tab performs one jump to the divider.
  document.addEventListener("visibilitychange", () => {
    if(document.visibilityState === "hidden"){
      if(active && stick) markRead(active, true);
      resetReadScroll();
      flushRead(active); // push the read watermark now, before the tab may freeze/close
    } else {
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
  resync().catch(() => {}).finally(() => changesLoop(host)); // snapshot, then the live channel (POST /api/changes)
}

applyTheme((() => { try { return localStorage.getItem("klax_theme2"); } catch(e){ return null; } })() || "light");
injectEmojiFont();
initAuth(start);
window.addEventListener("pageshow", resetMobileComposerFocus);
