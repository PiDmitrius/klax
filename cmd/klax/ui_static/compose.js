// compose.js — the message composer: textarea (auto-grow, Enter-to-send), staged
// attachments (paperclip / paste / drop, with chips), and send(). The composer never
// mutates the read model: the server is authoritative, and the sent message appears only
// when the live event / transcript reports it back. Composer state (text + staged files +
// retry nonce) is PER SESSION: app.js stashes it on tab switch and restores it on return,
// so a draft typed in one tab never shows up in another.

import { api, getToken } from "./base.js";

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
let files = [];                 // staged { file, name, url? } for the next send (url = lazy thumb blob)
let retryNonce = "";            // outbox nonce this live composer currently corresponds to ("" = none)
const drafts = {};              // created -> { text, files, nonce } — stashed composer state per tab
const recoveryQueues = {};      // created -> remaining recovered drafts, each retaining its ORIGINAL nonce

// --- Durable outbox: a no-loss guarantee for the TEXT of a submitted message. Every send mirrors its
// text to localStorage BEFORE the network request and clears it only once the server confirms the
// message durably enqueued (HTTP 204 == fsynced enq record). So at every instant the text is either in
// the server's durable queue OR here in the browser — a tab close/reload/crash during the in-flight
// send, or a failed send, can never drop it: recoverOutbox() restores survivors on the next load.
// Design notes (hardened after review):
//  - PER-NONCE KEYS, not one shared JSON array: two browser tabs never lose-update each other's
//    entries (each put/drop touches only its own key).
//  - NAMESPACED BY IDENTITY (the auth token): a browser shared by different users/tokens never
//    restores or sends another identity's text.
//  - CONFIRMED WRITES + NO EVICTION: outboxPut reports whether the write actually committed and never
//    evicts an unconfirmed entry. send() DURABLY STORES the text before transmitting and REFUSES to
//    send if it cannot (keeping the composer intact + notifying) — it never "sends anyway and hopes",
//    so a crash can never leave text neither on the server nor in the browser.
//  - FRESH NONCE ON EDIT: a nonce is reused only when its stored entry holds the exact text being
//    sent; an edited message gets a new nonce so the server can't dedupe the edit down to old text.
//    The reuse makes an idempotent resend of the SAME message safe (no double-delivery). Attachments
//    are out of scope (localStorage holds no blobs) — text is the guarantee.
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
    localStorage.setItem(key, JSON.stringify(entry));
    return localStorage.getItem(key) !== null; // confirm the write survived (quota errors throw or no-op)
  } catch(e){ return false; }
}
function outboxDrop(nonce){ if(nonce){ try { localStorage.removeItem(obKey(nonce)); } catch(e){} } }
function outboxList(){ const p = OB_PREFIX + idTag() + "."; const out = []; obScan(k => { if(k.indexOf(p) === 0){ try { const e = JSON.parse(localStorage.getItem(k)); if(e) out.push(e); } catch(_){} } }); return out; }

// syncRetry keeps the durable outbox copy in step with the user's live edits of a restored/failed
// message, so the text is NEVER momentarily un-stored (the earlier "drop on first edit" lost it on a
// crash mid-edit). Editing a message that was already TRANSMITTED (sent==true) rotates to a fresh
// never-sent nonce — writing the replacement BEFORE dropping the original — so the edited text is
// delivered (not deduped away) and stays durable throughout; editing a not-yet-sent draft updates it
// in place. Emptying the field discards the entry. A storage failure leaves the original untouched.
function syncRetry(text){
  if(!retryNonce) return;
  const t = (text || "").trim();
  const e = outboxGet(retryNonce);
  if(!e){ retryNonce = ""; return; }                  // entry gone elsewhere — detach
  if(e.text === t) return;                            // unchanged (e.g. an attach) — nothing to persist
  if(!t){ outboxDrop(retryNonce); retryNonce = ""; return; } // emptied → discarded
  if(e.sent){
    // Editing an already-TRANSMITTED message makes it a NEW message that must never resend under the
    // old, already-accepted nonce (the server would dedup it to the OLD text and the edit would
    // vanish). Rotate to a fresh nonce, but WRITE THE REPLACEMENT FIRST and drop the old copy ONLY if
    // it committed — so a durable copy always exists (the old A, or the new B), never neither. If the
    // replacement can't be stored, keep the old copy; send() then mints/stores fresh or refuses.
    const nn = newNonce();
    if(outboxPut({ created: e.created, text: t, nonce: nn, sent: false, at: Date.now() })){
      outboxDrop(retryNonce);
      retryNonce = nn;
    }
  } else {
    // Not-yet-transmitted draft: safe to update in place; a failed write leaves the prior text durable.
    outboxPut({ created: e.created, text: t, nonce: retryNonce, sent: false, at: e.at || Date.now() });
  }
}

// initCompose wires the composer DOM. deps: { getActive():created, isLive?(created),
// notice?(), onAfterSend?() }.
export function initCompose(deps){
  const ta = document.getElementById("input");
  const fileInput = document.getElementById("file");
  const bar = document.getElementById("cbar");
  if(ta){
    autoGrow(ta);
    ta.addEventListener("input", () => { syncRetry(ta.value); autoGrow(ta); });
    ta.addEventListener("keydown", e => { if(e.key === "Enter" && !e.shiftKey){ e.preventDefault(); send(deps); } });
    ta.addEventListener("paste", e => {
      let added = false;
      for(const it of (e.clipboardData && e.clipboardData.items) || []){
        if(it.kind === "file"){ const f = it.getAsFile(); if(f){ files.push({ file: f, name: f.name || "pasted.png" }); added = true; } }
      }
      if(added) renderChips();
    });
  }
  if(fileInput) fileInput.addEventListener("change", () => { for(const f of fileInput.files) files.push({ file: f, name: f.name }); fileInput.value = ""; renderChips(); });
  if(bar){
    ["dragover","dragenter"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); bar.classList.add("drag"); }));
    ["dragleave","drop"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); bar.classList.remove("drag"); }));
    bar.addEventListener("drop", e => { for(const f of (e.dataTransfer && e.dataTransfer.files) || []) files.push({ file: f, name: f.name }); renderChips(); });
  }
  const btn = document.getElementById("sendbtn");
  if(btn) btn.addEventListener("click", () => send(deps));
  const ab = document.getElementById("attachbtn");
  if(ab && fileInput) ab.addEventListener("click", () => fileInput.click());
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
  drafts[created] = { text: ta ? ta.value : "", files, nonce: retryNonce };
}
export function loadDraft(created){
  const d = created ? drafts[created] : null;
  if(created) delete drafts[created];
  files.forEach(releaseThumb);
  const ta = document.getElementById("input");
  if(ta){ ta.value = d ? d.text : ""; autoGrow(ta); }
  files = d ? d.files : [];
  retryNonce = d ? d.nonce : "";
  renderChips();
}
export function dropDraft(created){
  const d = drafts[created];
  if(d){ delete drafts[created]; d.files.forEach(releaseThumb); }
  delete recoveryQueues[created]; // durable originals stay in localStorage and become orphans on reload
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
      releaseThumb(f);
      files.splice(i, 1); renderChips();
    });
    chips.appendChild(c);
  });
}

async function send(deps){
  const ta = document.getElementById("input");
  const text = (ta ? ta.value : "").trim();
  const staged = files.slice();
  if(!text && !staged.length) return;
  const created = deps.getActive();
  if(!created) return;

  // Pick the nonce and DURABLY store the exact text BEFORE transmitting — the core of the guarantee:
  //  - Reuse retryNonce ONLY if its stored entry holds THIS exact text (an idempotent resend of the
  //    same message, so the server dedupes). A new or EDITED message gets a fresh nonce — it must
  //    never post under a nonce already transmitted for different text, or the server would dedupe it
  //    to the old text and the edit would vanish.
  //  - The text MUST commit to localStorage first. If it cannot (storage disabled/full), DO NOT send:
  //    keep the composer intact and tell the user it was not submitted, so the text is never lost.
  //    (We never "send anyway and hope" — that is the crash-loss the guarantee forbids.)
  let nonce;
  if(text){
    // Without a cryptographic nonce source we cannot guarantee tab-unique nonces, so a durable send
    // could collide with another tab and overwrite its message — refuse rather than risk losing text.
    if(!sendTabSecure){
      if(deps.notice) deps.notice("Браузер не поддерживает надёжную генерацию идентификаторов — отправка текста отключена во избежание потери. Обновите браузер.");
      return; // composer + retryNonce intact — nothing lost
    }
    const cur = retryNonce ? outboxGet(retryNonce) : null;
    // Reuse retryNonce when its stored entry still holds this exact text (idempotent resend), OR when
    // the entry is GONE while retryNonce is still set — that means this text was already transmitted
    // under retryNonce and its durable copy was dropped on a 204, e.g. delivered by ANOTHER tab that
    // held the same recovered draft. Reusing the nonce lets the server dedup it into a no-op instead
    // of minting a fresh nonce that would DUPLICATE-deliver. (An edit clears/rotates retryNonce via
    // syncRetry, so a set retryNonce with a missing entry always corresponds to THIS unmodified text.)
    const reuse = !!retryNonce && (!cur || cur.text === text);
    nonce = reuse ? retryNonce : newNonce();
    if(!outboxPut({ created, text, nonce, sent: true, at: (cur && cur.at) || Date.now() })){
      if(deps.notice) deps.notice("Не удалось сохранить сообщение локально — отправка отменена. Освободите хранилище браузера и повторите.");
      return; // composer + retryNonce left intact — nothing lost
    }
    if(!reuse && retryNonce) outboxDrop(retryNonce); // supersede the stale entry AFTER the new one is stored
  } else {
    nonce = retryNonce || newNonce(); // attachment-only: no text to durably guarantee
  }
  retryNonce = "";

  // Text is durably stored (or there is none) — safe to clear the composer optimistically.
  if(ta){ ta.value = ""; autoGrow(ta); }
  files = []; staged.forEach(releaseThumb); renderChips();
  if(deps.onAfterSend) deps.onAfterSend();

  try {
    let r;
    if(staged.length){
      const fd = new FormData();
      fd.append("session", String(created)); fd.append("text", text); fd.append("nonce", nonce);
      staged.forEach(f => fd.append("files", f.file, f.name));
      r = await api("/api/send", { method: "POST", body: fd });
    } else {
      r = await api("/api/send", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: created, text, nonce }) });
    }
    // Keep the outbox copy on ANY non-2xx: the message is still unconfirmed, so it stays durable
    // (and is shown again in the composer by rollback) until a successful resend clears it.
    if(!r.ok){ rollback(deps, created, text, staged, nonce, (await r.text()).trim() || "сообщение не принято"); return; }
    outboxDrop(nonce); // server accepted & fsynced it — the browser copy is no longer needed
    showNextRecovered(created, deps);
  } catch(e){
    rollback(deps, created, text, staged, nonce, "сеть недоступна — сообщение не отправлено"); // outbox copy stays
  }
}

// After one recovered message is confirmed, surface the next ORIGINAL message for that session.
// Never clobber text/files the user typed while the request was in flight; in that case the queue
// simply waits until a later successful send/tab restore.
function showNextRecovered(created, deps){
  const q = recoveryQueues[created];
  if(!q || !q.length) return;
  const next = q[0];
  if(deps.getActive() === created){
    const ta = document.getElementById("input");
    if(!ta || ta.value.trim() || files.length) return;
    q.shift(); ta.value = next.text; retryNonce = next.nonce; autoGrow(ta); renderChips();
  } else {
    const d = drafts[created];
    if(d && (d.text.trim() || d.files.length)) return;
    q.shift(); drafts[created] = next;
  }
  if(!q.length) delete recoveryQueues[created];
}

function rollback(deps, created, text, staged, nonce, msg){
  // Restore the failed draft into ITS session — the user may have switched tabs while the
  // send was in flight. Active tab: back into the live composer; other live tab: into its
  // stash. Never clobber a draft composed since, and never resurrect a draft for a
  // session closed while the send was in flight (that would leak the staged Files).
  if(deps.getActive() === created){
    const ta = document.getElementById("input");
    if(ta && !ta.value.trim() && !files.length){ ta.value = text || ""; autoGrow(ta); files = (staged || []).slice(); retryNonce = nonce || ""; renderChips(); }
  } else if(!deps.isLive || deps.isLive(created)){
    const d = drafts[created];
    if(!d || (!d.text.trim() && !d.files.length)) drafts[created] = { text: text || "", files: (staged || []).slice(), nonce: nonce || "" };
  }
  if(msg && deps.notice) deps.notice(msg);
}

// recoverOutbox restores every message that was submitted but never confirmed durable by the server
// (tab closed/reloaded/crashed mid-send, or a send that failed) back into its session's composer
// draft. Called ONCE at startup, before the first tab is selected, so the active tab's recovered text
// lands straight in the live composer via loadDraft. Returns the number of messages recovered. deps:
// { isLive(created), notice(text) }.
//
// Every live-session entry keeps its ORIGINAL nonce and remains a separate message. The first is
// shown in the composer; the rest queue behind it and surface one-by-one after a successful 204.
// This is the only exactly-once-safe recovery: the server can dedupe a request whose 204 was lost.
// Entries for a closed session are deliberately NOT re-homed — the original may already have run,
// and sending it to another session under a fresh nonce would duplicate side effects.
export function recoverOutbox(deps){
  const list = outboxList().sort((a, b) => (a.at || 0) - (b.at || 0));
  if(!list.length) return 0;
  const isLive = deps && deps.isLive;
  const groups = new Map(); // target created -> separate original drafts, in submission order
  let orphaned = 0;
  for(const e of list){
    if(!e || !e.text){ outboxDrop(e && e.nonce); continue; } // nothing recoverable (no text)
    if(isLive && !isLive(e.created)){ orphaned++; continue; }
    if(!groups.has(e.created)) groups.set(e.created, []);
    groups.get(e.created).push({ text: e.text, files: [], nonce: e.nonce });
  }
  let recovered = 0;
  for(const [target, entries] of groups){
    const d = drafts[target];
    if(d && (d.text.trim() || d.files.length)) continue; // a newer draft already occupies this composer — don't clobber
    drafts[target] = entries.shift();
    if(entries.length) recoveryQueues[target] = entries;
    recovered += 1 + entries.length;
  }
  if(recovered && deps && deps.notice){
    deps.notice("Восстановлено несохранённых сообщений: " + recovered);
  }
  if(orphaned && deps && deps.notice) deps.notice("Не восстановлено сообщений из закрытых сессий: " + orphaned);
  return recovered;
}
