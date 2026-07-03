// compose.js — the message composer: textarea (auto-grow, Enter-to-send), staged
// attachments (paperclip / paste / drop, with chips), and send(). The composer never
// mutates the read model: the server is authoritative, and the sent message appears only
// when the live event / transcript reports it back. Composer state (text + staged files +
// retry nonce) is PER SESSION: app.js stashes it on tab switch and restores it on return,
// so a draft typed in one tab never shows up in another.

import { api } from "./base.js";

const SENDTAB = (typeof crypto !== "undefined" && crypto.randomUUID) ? crypto.randomUUID().slice(0, 8) : "tab";
let nonceCtr = 0;
let files = [];                 // staged { file, name, url? } for the next send (url = lazy thumb blob)
let retryNonce = "";            // reused only for an unchanged draft restored after send failure
const drafts = {};              // created -> { text, files, nonce } — stashed composer state per tab

// initCompose wires the composer DOM. deps: { getActive():created, isLive?(created),
// notice?(), onAfterSend?() }.
export function initCompose(deps){
  const ta = document.getElementById("input");
  const fileInput = document.getElementById("file");
  const bar = document.getElementById("cbar");
  if(ta){
    autoGrow(ta);
    ta.addEventListener("input", () => { retryNonce = ""; autoGrow(ta); });
    ta.addEventListener("keydown", e => { if(e.key === "Enter" && !e.shiftKey){ e.preventDefault(); send(deps); } });
    ta.addEventListener("paste", e => {
      let added = false;
      for(const it of (e.clipboardData && e.clipboardData.items) || []){
        if(it.kind === "file"){ const f = it.getAsFile(); if(f){ retryNonce = ""; files.push({ file: f, name: f.name || "pasted.png" }); added = true; } }
      }
      if(added) renderChips();
    });
  }
  if(fileInput) fileInput.addEventListener("change", () => { retryNonce = ""; for(const f of fileInput.files) files.push({ file: f, name: f.name }); fileInput.value = ""; renderChips(); });
  if(bar){
    ["dragover","dragenter"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); bar.classList.add("drag"); }));
    ["dragleave","drop"].forEach(ev => bar.addEventListener(ev, e => { e.preventDefault(); bar.classList.remove("drag"); }));
    bar.addEventListener("drop", e => { retryNonce = ""; for(const f of (e.dataTransfer && e.dataTransfer.files) || []) files.push({ file: f, name: f.name }); renderChips(); });
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
  if(!d) return;
  delete drafts[created];
  d.files.forEach(releaseThumb);
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
      retryNonce = "";
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
  const nonce = retryNonce || (SENDTAB + "-" + (++nonceCtr));
  retryNonce = "";

  // clear the composer immediately (thumbs released — a rollback re-mints them lazily)
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
    if(!r.ok){ rollback(deps, created, text, staged, nonce, (await r.text()).trim() || "сообщение не принято"); return; }
  } catch(e){
    rollback(deps, created, text, staged, nonce, "сеть недоступна — сообщение не отправлено");
  }
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
