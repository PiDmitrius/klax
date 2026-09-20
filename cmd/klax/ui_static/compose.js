// compose.js — the message composer: textarea (desktop Enter-to-send; mobile Enter-to-newline), staged
// attachments (paperclip / paste / drop, with chips), and send(). The composer never
// mutates the read model: the server is authoritative, and the sent message appears only
// when the live event / transcript reports it back. Composer state (text + staged files +
// retry nonce) is PER SESSION: app.js stashes it on tab switch and restores it on return,
// so a draft typed in one tab never shows up in another. One request may be in flight per session;
// the composer is read-only during it, and only HTTP 204 clears the submitted draft.

import { api, getToken, hasCoarsePointer, bindButtonActivation } from "./base.js";

// Phones have no practical Shift+Enter gesture, so their primary coarse pointer changes plain Enter
// into a newline and leaves sending to the visible button. Ctrl/Cmd+Enter remains an explicit send
// everywhere (including a physical keyboard attached to a phone/tablet).
export function composerEnterSends(e, coarse){
  if(!e || e.key !== "Enter" || e.isComposing) return false;
  if(e.ctrlKey || e.metaKey) return true;
  return !coarse && !e.shiftKey;
}

// A per-page-load tab id that seeds every nonce. It must be collision-resistant even without
// crypto.randomUUID — a constant would make two tabs mint the same nonce (same per-key + server
// idempotency key) and lose one message. Prefer crypto.getRandomValues (128-bit); with no crypto at
// all, combine a persisted per-origin boot counter + high-res time + Math.random so two tabs opened
// the same millisecond still differ. Best-effort, not a hard guarantee — but the durable send path
// refuses anything it cannot store, and per-nonce keys turn a same-key clash into an overwrite, not
// silent corruption.
let sendTabSecure = false; // did SENDTAB come from a cryptographic source (→ collision-safe nonces)?
function randTab(){
  try {
    if(typeof crypto !== "undefined" && crypto.randomUUID){ sendTabSecure = true; return crypto.randomUUID(); }
    if(typeof crypto !== "undefined" && crypto.getRandomValues){ sendTabSecure = true; const a = new Uint32Array(4); crypto.getRandomValues(a); return Array.from(a, x => x.toString(36)).join(""); }
  } catch(e){}
  // No crypto at all (essentially never — getRandomValues works even on plain http). This id is only
  // best-effort-unique, so sendTabSecure stays false and send() REFUSES to durably submit text: a
  // probabilistic tab-id collision could overwrite another tab's message, which the no-loss guarantee
  // forbids. We would rather block sending than risk losing text.
  let boot = 0;
  try { boot = (parseInt(localStorage.getItem("klax_boot") || "0", 10) || 0) + 1; localStorage.setItem("klax_boot", String(boot)); } catch(e){}
  const hi = (typeof performance !== "undefined" && performance.now) ? Math.floor(performance.now() * 1000) : 0;
  return "t" + boot.toString(36) + "-" + Date.now().toString(36) + hi.toString(36) + Math.floor(Math.random() * 1e9).toString(36);
}
const SENDTAB = randTab();
let nonceCtr = 0;
function newNonce(){ return SENDTAB + "-" + (++nonceCtr); }
const pendingSends = new Map();
let getActive = () => 0, accessReadOnly = false;
let files = [];                 // staged { file, name, url? } for the next send (url = lazy thumb blob)
let attachmentsChanged = false;
let retryNonce = "";            // outbox nonce this live composer currently corresponds to ("" = none)
const drafts = {};              // created -> { text, files, nonce } — stashed composer state per tab
const recoveryQueues = {};      // created -> remaining recovered drafts, each retaining its ORIGINAL nonce

// One durable outbox holds typed and submitted text, scoped by identity. Text is immutable per
// nonce: edits write a new entry before removing the previous one, so browser tabs cannot overwrite
// each other's edits. Sending marks the same entry sent; retries keep its nonce. Only acceptance or
// an explicit edit/discard removes it. Storage failure preserves the prior entry and blocks sending.
// Attachments remain in memory; localStorage stores no blobs.
const OB_PREFIX = "klax_ob.";
const OB_CAP = 500; // hard bound on retained unconfirmed entries per identity (never evicted — refused beyond)
// idTag: a cheap, stable per-identity tag (djb2 of the auth token) so outbox keys are scoped to the
// authenticated user and cannot leak across a token change on a shared browser.
function idTag(){ const t = getToken() || ""; let h1 = 5381, h2 = 52711; for(let i = 0; i < t.length; i++){ const c = t.charCodeAt(i); h1 = ((h1 << 5) + h1 + c) >>> 0; h2 = ((h2 << 5) + h2 + (c ^ 0x9e)) >>> 0; } return h1.toString(36) + h2.toString(36); }
function obKey(nonce){ return OB_PREFIX + idTag() + "." + nonce; }
function obScan(fn){ try { for(let i = 0; i < localStorage.length; i++){ const k = localStorage.key(i); if(k) fn(k); } } catch(e){} }
function outboxGet(nonce){ try { const v = localStorage.getItem(obKey(nonce)); return v ? JSON.parse(v) : null; } catch(e){ return null; } }
function outboxCount(){ const p = OB_PREFIX + idTag() + "."; let n = 0; obScan(k => { if(k.indexOf(p) === 0) n++; }); return n; }
// outboxPut writes one entry and returns TRUE only if it actually committed to localStorage; it never
// evicts to make room (refuses beyond the cap). Callers must treat FALSE as "not durably stored".
function outboxPut(entry){
  try {
    const key = obKey(entry.nonce);
    if(localStorage.getItem(key) === null && outboxCount() >= OB_CAP) return false; // full — never drop an unconfirmed message
    const value = JSON.stringify(entry);
    localStorage.setItem(key, value);
    return localStorage.getItem(key) === value; // confirm the write survived (quota errors throw or no-op)
  } catch(e){ return false; }
}
function outboxDrop(nonce){
  if(!nonce) return true;
  try { const key = obKey(nonce); localStorage.removeItem(key); return localStorage.getItem(key) === null; }
  catch(e){ return false; }
}
function discardUnsent(entry){ return entry.sent === false && outboxDrop(entry.nonce); }
export function outboxList(){ const p = OB_PREFIX + idTag() + "."; const out = []; obScan(k => { if(k.indexOf(p) === 0){ try { const e = JSON.parse(localStorage.getItem(k)); if(e) out.push(e); } catch(_){} } }); return out; }

function persistDraft(created, text, transmitted = false){
  if(!created) return false;
  const previous = retryNonce;
  const cur = previous ? outboxGet(previous) : null;
  if(!text){
    if(cur && !outboxDrop(previous)) return false;
    retryNonce = transmitted ? (!attachmentsChanged && previous) || newNonce() : "";
    attachmentsChanged = false;
    return true;
  }
  if(!sendTabSecure) return false;
  if(!transmitted && !attachmentsChanged && cur && cur.text === text) return true;
  const reuse = transmitted && !attachmentsChanged && previous && (!cur || cur.text === text);
  const nonce = reuse ? previous : newNonce();
  if(!outboxPut({ created, text, nonce, sent: transmitted, at: (cur && cur.at) || Date.now() })){
    if(!cur && !transmitted) retryNonce = "";
    return false;
  }
  retryNonce = nonce;
  attachmentsChanged = false;
  if(previous && previous !== nonce) outboxDrop(previous);
  return true;
}

export function updateComposerAccess(readOnly){
  accessReadOnly = readOnly;
  const ta = document.getElementById("input");
  const sending = pendingSends.get(getActive());
  const locked = readOnly || !!sending;
  const canCancel = !!sending && sending.cancelReady;
  if(ta){
    // Mobile editing resumes through a fresh user tap, never through focus retained across a lock.
    if(locked && hasCoarsePointer() && document.activeElement === ta) ta.blur();
    if(ta.readOnly !== locked) ta.readOnly = locked;
  }
  const bar = document.getElementById("cbar");
  if(bar){
    bar.classList.toggle("read-only", locked);
    bar.setAttribute("aria-busy", String(!!sending));
    bar.inert = readOnly;
    bar.querySelectorAll("input, textarea, button").forEach(el => {
      const disabled = readOnly || (!!sending && el.id !== "input" && !(el.id === "sendbtn" && canCancel));
      if(el.disabled !== disabled) el.disabled = disabled;
    });
  }
  const btn = document.getElementById("sendbtn");
  if(btn){
    btn.classList.toggle("cancel-send", canCancel);
    btn.title = canCancel ? "Отменить ожидание отправки" : sending ? "Отправка…" : "Отправить";
    btn.setAttribute("aria-label", btn.title);
  }
}

export function initCompose(deps){
  getActive = deps.getActive;
  const ta = document.getElementById("input");
  const fileInput = document.getElementById("file");
  const bar = document.getElementById("cbar");
  let storageFailed = false;
  if(ta){
    autoGrow(ta);
    ta.addEventListener("pointerdown", e => { if(ta.readOnly && hasCoarsePointer()) e.preventDefault(); });
    ta.addEventListener("focus", () => { if(ta.readOnly && hasCoarsePointer()) ta.blur(); });
    ta.addEventListener("beforeinput", e => { if(pendingSends.has(getActive()) || deps.readOnly()) e.preventDefault(); });
    ta.addEventListener("input", () => {
      autoGrow(ta);
      if(pendingSends.has(getActive()) || deps.readOnly()) return;
      const created = deps.getActive();
      const saved = persistDraft(created, ta.value);
      if(!saved && !storageFailed && deps.notice) deps.notice("Не удалось сохранить черновик в браузере. Не закрывайте страницу; скопируйте текст.");
      storageFailed = !saved;
      if(saved && !ta.value && !files.length) showNextRecovered(created, deps);
    });
    ta.addEventListener("keydown", e => { if(composerEnterSends(e, hasCoarsePointer())){ e.preventDefault(); send(deps); } });
    ta.addEventListener("paste", e => {
      if(pendingSends.has(getActive()) || deps.readOnly()){ e.preventDefault(); return; }
      let added = false;
      for(const it of (e.clipboardData && e.clipboardData.items) || []){
        if(it.kind === "file"){ const f = it.getAsFile(); if(f){ files.push({ file: f, name: f.name || "pasted.png" }); added = true; } }
      }
      if(added){ attachmentsChanged = true; renderChips(); }
    });
  }
  if(fileInput) fileInput.addEventListener("change", () => { if(pendingSends.has(getActive()) || deps.readOnly()) return; for(const f of fileInput.files){ files.push({ file: f, name: f.name }); attachmentsChanged = true; } fileInput.value = ""; renderChips(); });
  if(bar){
    ["dragover","dragenter"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); if(pendingSends.has(getActive()) || deps.readOnly()) return; bar.classList.add("drag"); }));
    ["dragleave","drop"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); bar.classList.remove("drag"); }));
    bar.addEventListener("drop", e => { if(pendingSends.has(getActive()) || deps.readOnly()) return; for(const f of (e.dataTransfer && e.dataTransfer.files) || []){ files.push({ file: f, name: f.name }); attachmentsChanged = true; } renderChips(); });
  }
  const btn = document.getElementById("sendbtn");
  if(btn) bindButtonActivation(btn, () => {
    if(deps.readOnly()) return;
    const sending = pendingSends.get(getActive());
    if(sending){ if(sending.cancelReady) sending.controller.abort("cancelled"); }
    else send(deps);
  });
  const ab = document.getElementById("attachbtn");
  if(ab && fileInput) ab.addEventListener("click", () => { if(!pendingSends.has(getActive()) && !deps.readOnly()) fileInput.click(); });
}

function autoGrow(ta){
  const sizer = document.getElementById("inputSizer");
  if(sizer) sizer.textContent = (ta && ta.value ? ta.value : "") + "\u200b";
}

// Thumb blob URLs are cached on the staged entry and re-created lazily after a release,
// so tab switches and send/rollback cycles do not leak object URLs.
function thumbURL(f){
  if(!f.url && /^image\//.test(f.file.type)) f.url = URL.createObjectURL(f.file);
  return f.url || "";
}
function releaseThumb(f){ if(f.url){ URL.revokeObjectURL(f.url); delete f.url; } }

// saveDraft/loadDraft move the live composer state to/from the per-session stash on tab
// switches; dropDraft forgets a closed session's draft. loadDraft releases the thumbs of
// whatever was live: if that state was stashed, its thumbs are re-minted on restore.
export function saveDraft(created){
  if(!created) return;
  const ta = document.getElementById("input");
  drafts[created] = { text: ta ? ta.value : "", files, nonce: retryNonce, attachmentsChanged };
}
export function loadDraft(created){
  const d = created ? drafts[created] : null;
  if(created) delete drafts[created];
  files.forEach(releaseThumb);
  const ta = document.getElementById("input");
  if(ta){ ta.value = d ? d.text : ""; autoGrow(ta); }
  files = d ? d.files : [];
  retryNonce = d ? d.nonce : "";
  attachmentsChanged = !!(d && d.attachmentsChanged);
  renderChips();
}
export function dropDraft(created, confirmedDeletion = false){
  const d = drafts[created];
  if(d){ delete drafts[created]; d.files.forEach(releaseThumb); }
  delete recoveryQueues[created];
  if(confirmedDeletion){ for(const entry of outboxList()){ if(entry.created === created) discardUnsent(entry); } }
}

function renderChips(){
  const chips = document.getElementById("chips");
  if(!chips) return;
  chips.classList.toggle("hidden", files.length === 0);
  chips.innerHTML = "";
  files.forEach((f, i) => {
    const c = document.createElement("div");
    c.className = "chip";
    const thumb = thumbURL(f);
    c.innerHTML = (thumb ? '<img alt="">' : '<span class="ic">📎</span>') + '<span class="nm"></span><span class="rm">✕</span>';
    c.querySelector(".nm").textContent = f.name;
    if(thumb) c.querySelector("img").src = thumb;
    c.querySelector(".rm").addEventListener("click", () => {
      if(pendingSends.has(getActive()) || accessReadOnly) return;
      releaseThumb(f);
      files.splice(i, 1); attachmentsChanged = true; renderChips();
    });
    chips.appendChild(c);
  });
}

async function send(deps){
  if(pendingSends.has(getActive()) || deps.readOnly()) return;
  const ta = document.getElementById("input");
  const text = ta ? ta.value : "";
  const staged = files.slice();
  if(!text.trim() && !staged.length) return;
  const created = deps.getActive();
  if(!created) return;

  if(!persistDraft(created, text, true)){
    if(deps.notice) deps.notice("Не удалось сохранить сообщение локально — отправка отменена. Черновик остаётся во вводе.");
    return;
  }
  const nonce = retryNonce;
  const controller = new AbortController();
  const sending = { controller, cancelReady: false };
  pendingSends.set(created, sending);
  updateComposerAccess(accessReadOnly);
  const cancelTimer = setTimeout(() => {
    if(pendingSends.get(created) !== sending) return;
    sending.cancelReady = true;
    updateComposerAccess(accessReadOnly);
  }, 3000);
  const timer = setTimeout(() => controller.abort(), 60000);
  let accepted = false;
  try {
    let r;
    if(staged.length){
      const fd = new FormData();
      fd.append("session", String(created)); fd.append("text", text); fd.append("nonce", nonce);
      staged.forEach(f => fd.append("files", f.file, f.name));
      r = await api("/api/send", { method: "POST", body: fd, signal: controller.signal });
    } else {
      r = await api("/api/send", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: created, text, nonce }), signal: controller.signal });
    }
    if(controller.signal.aborted) return;
    accepted = r.status === 204;
    if(!accepted && deps.notice) deps.notice((await r.text()).trim() || "Отправка не подтверждена — сообщение сохранено во вводе");
  } catch(e){
    if(deps.notice) deps.notice(controller.signal.reason === "cancelled"
      ? "Ожидание отменено. Черновик сохранён; сообщение могло быть принято сервером."
      : "Отправка не подтверждена — сообщение сохранено во вводе. Можно повторить отправку.");
  } finally {
    clearTimeout(timer);
    clearTimeout(cancelTimer);
    pendingSends.delete(created);
    updateComposerAccess(accessReadOnly);
  }
  if(!accepted) return;
  outboxDrop(nonce);
  if(deps.getActive() === created){
    if(ta){ ta.value = ""; autoGrow(ta); }
    files.forEach(releaseThumb); files = []; retryNonce = ""; renderChips();
    if(deps.onAfterSend) deps.onAfterSend();
  } else {
    const d = drafts[created];
    if(d){ d.files.forEach(releaseThumb); delete drafts[created]; }
  }
  showNextRecovered(created, deps);
}

// Each confirmed recovery reveals the next original message with its own retry nonce.
function showNextRecovered(created, deps){
  const q = recoveryQueues[created];
  if(!q || !q.length) return;
  const next = q[0];
  if(deps.getActive() === created){
    const ta = document.getElementById("input");
    if(!ta || ta.value || files.length) return;
    q.shift(); ta.value = next.text; retryNonce = next.nonce; autoGrow(ta); renderChips();
  } else {
    const d = drafts[created];
    if(d && (d.text || d.files.length)) return;
    q.shift(); drafts[created] = next;
  }
  if(!q.length) delete recoveryQueues[created];
}

// recoverOutbox restores typed and submitted text before the first session is selected;
// loadDraft then moves it into the live composer. Returns the recovered count. deps:
// { isLive(created), notice(text) }.
//
// Every live-session entry keeps its ORIGINAL nonce and remains a separate message. The first is
// shown in the composer; the rest surface after acceptance or explicit discard of the current text.
// This is the only exactly-once-safe recovery: the server can dedupe a request whose 204 was lost.
// Confirmed deletion discards never-transmitted drafts. Startup cleanup only considers entries
// observed before the session-list request; newer entries cannot be judged by that snapshot.
// Submitted or legacy entries remain
// recoverable in storage; re-homing them could duplicate already accepted work.
export function recoverOutbox(deps, observedBeforeRequest = []){
  const discardable = new Set(observedBeforeRequest.map(e => e.nonce));
  const list = outboxList().sort((a, b) => (a.at || 0) - (b.at || 0));
  if(!list.length) return 0;
  const isLive = deps && deps.isLive;
  const groups = new Map(); // target created -> separate original drafts, in submission order
  let orphaned = 0;
  for(const e of list){
    if(!e || !e.text){ outboxDrop(e && e.nonce); continue; } // nothing recoverable (no text)
    if(isLive && !isLive(e.created)){
      if(!discardable.has(e.nonce) || !discardUnsent(e)) orphaned++;
      continue;
    }
    if(!groups.has(e.created)) groups.set(e.created, []);
    groups.get(e.created).push({ text: e.text, files: [], nonce: e.nonce });
  }
  let recovered = 0;
  for(const [target, entries] of groups){
    const d = drafts[target];
    if(d && (d.text || d.files.length)) continue; // a newer draft already occupies this composer — don't clobber
    drafts[target] = entries.shift();
    if(entries.length) recoveryQueues[target] = entries;
    recovered += 1 + entries.length;
  }
  if(recovered && deps && deps.notice){
    deps.notice("Восстановлено черновиков: " + recovered);
  }
  if(orphaned && deps && deps.notice) deps.notice("Сохранено сообщений без доступной сессии: " + orphaned);
  return recovered;
}
