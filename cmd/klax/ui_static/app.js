// app.js — bootstrap + wiring. Owns the model, the active-session render flow, and the
// poll host (cursor/lastSeq/onReload/onAffected); ties compose + tabs together. The whole
// old state machine (runningTurn/doneTurns/queuedTurns/tmpTurn/renderedPending/readMark/
// insertAnswer/breakMerge) is gone — a turn's truth is model turn.state.

import { TurnModel } from "./model.js";
import { renderSession } from "./render.js";
import { pollLoop } from "./events.js";
import { api, getToken, setToken } from "./base.js";
import { selectionInLog, toBottom } from "./scroll.js";
import { initCompose } from "./compose.js";
import { initTabs, reconcileSessions, renderTabs } from "./tabs.js";
import { injectEmojiFont } from "./emoji.js";

const model = new TurnModel();
const loaded = {};        // created -> transcript loaded?
const readThrough = {};   // created -> highest event seq the user has actually read
const unreadJump = {};    // created -> one-shot scroll to the unread divider
const readGraceUntil = {}, readGraceTimer = {};
const READ_GRACE_MS = 1600;
let active = 0;
let cursor = null, lastSeq = 0;
let stick = true, pendingRender = false, readOnScroll = true;
let sessionList = []; // last /api/sessions list — for hash-change validity + lookups
const offsetFor = {}, moreFor = {}; // created -> first-loaded turn index + has-older-history flag (pagination)
const scrollTopFor = {}; // created -> last scrollTop, so tab switches do not snap by a pixel
const watchedImages = new WeakSet();

function logcol(){ return document.getElementById("logcol"); }
function getActive(){ return active; }
function seqOf(c){ const i = String(c || "").indexOf("-"); return i >= 0 ? (parseInt(String(c).slice(i + 1), 10) || 0) : 0; }
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
      rerender(created);
    }
  }, READ_GRACE_MS + 40);
}
function markRead(created, force){
  if(!created) return false;
  if(!force && inReadGrace(created)) return false;
  const visualChange = rawUnreadCount(created) > 0 || unreadJump[created] !== undefined || readGraceUntil[created] !== undefined;
  readThrough[created] = Math.max(readThrough[created] || 0, lastSeq);
  delete unreadJump[created];
  clearReadGrace(created);
  readOnScroll = true;
  return visualChange;
}
function markLoadedRead(created, watermark){
  if(!created) return;
  const seq = watermark !== undefined ? watermark : lastSeq;
  readThrough[created] = Math.max(readThrough[created] || 0, seq);
  delete unreadJump[created];
  clearReadGrace(created);
  readOnScroll = true;
}
function jumpToUnread(created){ if(created){ unreadJump[created] = true; startReadGrace(created); } }
function focusComposer(){
  const input = document.getElementById("input");
  if(input && documentVisible()) input.focus({ preventScroll: true });
}
function stickToBottom(){
  const sc = document.getElementById("log");
  if(sc) toBottom(sc);
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
  stick = (log.scrollHeight - log.scrollTop - log.clientHeight < 80);
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

function rerender(created){
  if(created !== active) return;
  const col = logcol();
  if(!col) return;
  if(selectionInLog(col)){ pendingRender = true; return; } // don't collapse a live selection
  renderSession(col, model.turns(active), readThrough[active], abortActive);
  watchInlineImages(col);
  if(moreFor[active]){ // older history exists → a "load earlier" button at the top
    const m = document.createElement("button");
    m.id = "more"; m.textContent = "↑ Загрузить раньше";
    m.addEventListener("click", () => loadOlder(active));
    col.insertBefore(m, col.firstChild);
  }
  if(unreadJump[active] && rawUnreadCount(active) > 0){
    const dv = col.querySelector(".readline");
    if(dv){
      readOnScroll = false;
      dv.scrollIntoView({ block: "start" });
      stick = false;
      delete unreadJump[active];
    }
  } else if(stick) stickToBottom();
  toggleToBottom();
}

function abortActive(){
  if(active) api("/api/abort", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: active }) }).catch(()=>{});
}

async function loadTranscript(created){
  try {
    const r = await api("/api/transcript?session=" + created);
    if(!r.ok) return;
    const data = await r.json();
    model.reconcile(created, data.turns || []);
    loaded[created] = true;
    offsetFor[created] = data.offset || 0;
    moreFor[created] = !!data.more;
    // The poll cursor is GLOBAL (one stream for all the user's sessions). Only the FIRST
    // load (cursor still null) establishes the baseline from the watermark (§A3); a later
    // lazy tab-load must NOT move the global cursor, or it would skip events for other
    // already-loaded sessions. Re-applied events for this session dedup by seq + block id.
    if(cursor === null && data.watermark){ cursor = data.watermark; lastSeq = seqOf(data.watermark); }
    markLoadedRead(created, data.watermark ? seqOf(data.watermark) : undefined); // a freshly (re)loaded tab is read
    rerender(created);
  } catch(e){}
}

// loadOlder pages in the previous turn-page and PREPENDS it, keeping the viewport stable
// (the scroll position is nudged by the height the prepended content added).
async function loadOlder(created){
  if(!offsetFor[created]) return; // nothing older
  const log = document.getElementById("log");
  const oldH = log ? log.scrollHeight : 0;
  try {
    const r = await api("/api/transcript?session=" + created + "&before=" + offsetFor[created]);
    if(!r.ok) return;
    const data = await r.json();
    model.prepend(created, data.turns || []);
    offsetFor[created] = data.offset || 0;
    moreFor[created] = !!data.more;
    const prev = stick; stick = false; // never snap to the bottom after loading old history
    rerender(created);
    stick = prev;
    if(log) log.scrollTop += log.scrollHeight - oldH;
  } catch(e){}
}

// rawUnreadCount is the true unread model, used for the in-log divider/jump. unreadCount
// is the tab/title display variant, suppressing the active visible tab's badge.
function rawUnreadCount(created){
  const base = readThrough[created];
  if(base === undefined) return 0;
  let n = 0;
  for(const t of model.turns(created)){
    if(t.role !== "user"){
      if(t.eventSeq !== undefined && t.eventSeq > base) n++;
      continue;
    }
    for(const b of (t.blocks || [])) if(b.eventSeq !== undefined && b.eventSeq > base) n++;
  }
  return n;
}
function unreadCount(created){
  if(sameSession(created, active) && documentVisible()) return 0;
  return rawUnreadCount(created);
}
function advanceReadThroughPastViewport(log){
  if(!active || readThrough[active] === undefined || !log || inReadGrace(active)) return false;
  const top = log.getBoundingClientRect().top;
  let next = readThrough[active];
  log.querySelectorAll("[data-max-event-seq]").forEach(el => {
    const seq = parseInt(el.dataset.maxEventSeq || "0", 10) || 0;
    if(seq > next && el.getBoundingClientRect().bottom < top + 1) next = seq;
  });
  if(next <= readThrough[active]) return false;
  readThrough[active] = next;
  if(rawUnreadCount(active) === 0) delete unreadJump[active];
  else startReadGrace(active);
  return true;
}

async function selectSession(created){
  if(active && active !== created) rememberScroll(active);
  if(active && active !== created && documentVisible() && stick){
    markRead(active);
  }
  const hadUnread = loaded[created] && rawUnreadCount(created) > 0;
  active = created;
  if(location.hash !== "#" + created) location.hash = String(created);
  // Returning to an already-loaded tab with unread → jump to the "новые сообщения" divider
  // instead of the bottom; otherwise stick to the bottom.
  if(hadUnread) jumpToUnread(created);
  else markRead(created);
  stick = !hadUnread;
  renderTabs(active);
  if(!loaded[created]) await loadTranscript(created);
  else {
    rerender(created);
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
  sessionList = list;
  if(active && !list.some(s => s.created === active)){
    model.drop(active); delete loaded[active]; markRead(active);
    active = 0;
  }
  reconcileSessions(list, active);
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
  if(active){ model.appendStandalone(active, { role: "notice", text: t }); rerender(active); }
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

// the poll host events.js drives
const host = {
  model,
  ctx: { onSessions: list => { onSessionsList(list); }, onNotice: onNoticeEvent },
  cursor: () => cursor, setCursor: c => { cursor = c; },
  lastSeq: () => lastSeq, setLastSeq: n => { lastSeq = n; },
  onAffected: set => {
    for(const c of set){
      if(c === active){
        if(documentVisible() && stick){
          markRead(c);
        } else if(rawUnreadCount(c) > 0){
          stick = false;
          startReadGrace(c);
        }
        rerender(c);
      }
    }
    renderTabs(active);
  },
  onAuthFail: () => { const a = document.getElementById("app"); if(a) a.classList.remove("active"); const g = document.getElementById("gate"); if(g) g.classList.remove("hidden"); },
  onRestart: () => showNotice("klax обновился"),
  onReload: async () => {
    // Uncoverable cursor: invalidate EVERYTHING and reset the global cursor, then
    // re-establish from the server (the active load re-commits the watermark since
    // cursor is null). Inactive loaded sessions are dropped so they reload fresh on
    // their next selection rather than keeping a model stale past the gap.
    Object.keys(loaded).forEach(k => { model.drop(Number(k)); delete loaded[k]; });
    Object.keys(readThrough).forEach(k => delete readThrough[k]);
    Object.keys(unreadJump).forEach(k => delete unreadJump[k]);
    Object.keys(readGraceUntil).forEach(k => clearReadGrace(Number(k)));
    cursor = null; lastSeq = 0;
    await syncSessions();
    if(active && !loaded[active]) await loadTranscript(active);
  },
};

async function onNewSession(created){ await syncSessions(); await selectSession(created); }
async function afterClose(created){
  model.drop(created); delete loaded[created]; markRead(created);
  if(created === active) active = 0;
  await syncSessions();
}

function start(){
  document.getElementById("gate").classList.add("hidden");
  const app = document.getElementById("app"); if(app) app.classList.add("active");
  initCompose({
    getActive, notice: showNotice,
    onAfterSend: () => { stick = true; markRead(active, true); renderTabs(active); stickToBottom(); },
  });
  initTabs({ select: selectSession, onNew: onNewSession, afterClose, notice: showNotice, unread: unreadCount });
  // Delegated copy button for code fences (markdown emits <button class="copy">).
  const lw = document.getElementById("log");
  if(lw) lw.addEventListener("click", e => {
    const btn = e.target.closest && e.target.closest(".copy");
    if(!btn) return;
    const code = btn.closest("pre") && btn.closest("pre").querySelector("code");
    if(!code) return;
    const text = code.textContent || "";
    const done = () => { btn.classList.add("done"); btn.textContent = "✓"; setTimeout(() => { btn.classList.remove("done"); btn.textContent = "⧉"; }, 1200); };
    if(navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done).catch(() => fallbackCopy(text, done));
    else fallbackCopy(text, done);
  });
  document.addEventListener("selectionchange", () => { if(pendingRender && !selectionInLog(logcol())){ pendingRender = false; rerender(active); } });
  const log = document.getElementById("log");
  const allowReadOnScroll = () => { readOnScroll = true; };
  if(log){
    log.addEventListener("wheel", allowReadOnScroll, { passive: true });
    log.addEventListener("touchstart", allowReadOnScroll, { passive: true });
  }
  if(log) log.addEventListener("scroll", () => {
    stick = (log.scrollHeight - log.scrollTop - log.clientHeight < 80);
    if(active) scrollTopFor[active] = log.scrollTop;
    if(readOnScroll && active && documentVisible()){
      const oldTop = log.scrollTop;
      const oldHeight = log.scrollHeight;
      const advanced = advanceReadThroughPastViewport(log);
      if(stick){
        if(markRead(active) || advanced){
          renderTabs(active);
          rerender(active);
        }
      } else if(advanced){
        renderTabs(active);
        rerender(active);
        log.scrollTop = oldTop + (log.scrollHeight - oldHeight);
        scrollTopFor[active] = log.scrollTop;
      }
    }
    toggleToBottom();
  });
  const th = document.getElementById("theme");
  if(th) th.addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark"));
  const tb = document.getElementById("tobottom");
  if(tb) tb.addEventListener("click", () => { stick = true; markRead(active, true); renderTabs(active); rerender(active); });
  window.addEventListener("hashchange", () => { const w = parseInt(location.hash.slice(1), 10); if(w && w !== active && sessionList.some(s => s.created === w)) selectSession(w); });
  document.addEventListener("keydown", e => {
    if(["ArrowDown","PageDown","End"," "].includes(e.key)) allowReadOnScroll();
  });
  // Read-through advances only while the user can actually see the bottom. Hidden tabs
  // stop advancing; foregrounding an unread active tab performs one jump to the divider.
  document.addEventListener("visibilitychange", () => {
    if(document.visibilityState === "hidden"){
      if(active && stick) markRead(active);
    } else {
      syncSessions();
      if(active){
        if(rawUnreadCount(active) > 0){
          stick = false;
          jumpToUnread(active);
        } else {
          markRead(active);
        }
        rerender(active);
      }
    }
  });
  syncSessions().then(() => pollLoop(host));
}

function gateSubmit(){ const el = document.getElementById("token"); const t = el ? el.value.trim() : ""; if(t){ setToken(t); start(); } }

applyTheme((() => { try { return localStorage.getItem("klax_theme2"); } catch(e){ return null; } })() || "light");
injectEmojiFont();
if(getToken()) start();
else {
  const btn = document.getElementById("tokenbtn"); if(btn) btn.addEventListener("click", gateSubmit);
  const tk = document.getElementById("token"); if(tk) tk.addEventListener("keydown", e => { if(e.key === "Enter") gateSubmit(); });
}
