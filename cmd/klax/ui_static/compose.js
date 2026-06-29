// compose.js — the message composer: textarea (auto-grow, Enter-to-send), staged
// attachments (paperclip / paste / drop, with chips), and send(). The visible echo is
// added only after /api/send returns a durable {seq,state}; this avoids a local optimistic
// bubble being briefly removed by a concurrent transcript/session reconcile.

import { api } from "./base.js";

const SENDTAB = (typeof crypto !== "undefined" && crypto.randomUUID) ? crypto.randomUUID().slice(0, 8) : "tab";
let nonceCtr = 0;
let files = [];                 // staged { file, name } for the next send
const blobsByNonce = new Map(); // nonce -> [blobURL] for rendered local previews

// initCompose wires the composer DOM. deps: { model, getActive():created, rerender(created),
// myNonces:Map, onBeforeSend?(), onAfterSend?() }.
export function initCompose(deps){
  const ta = document.getElementById("input");
  const fileInput = document.getElementById("file");
  const bar = document.getElementById("cbar");
  if(ta){
    ta.addEventListener("input", () => autoGrow(ta));
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
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight + 2, 240) + "px";
}

function renderChips(){
  const chips = document.getElementById("chips");
  if(!chips) return;
  chips.classList.toggle("hidden", files.length === 0);
  chips.innerHTML = "";
  files.forEach((f, i) => {
    const c = document.createElement("div");
    c.className = "chip";
    const isImg = /^image\//.test(f.file.type);
    c.innerHTML = (isImg ? '<img alt="">' : '<span class="ic">📎</span>') + '<span class="nm"></span><span class="rm">✕</span>';
    c.querySelector(".nm").textContent = f.name;
    if(isImg) c.querySelector("img").src = URL.createObjectURL(f.file);
    c.querySelector(".rm").addEventListener("click", () => {
      if(isImg){ const u = c.querySelector("img").src; if(u.startsWith("blob:")) URL.revokeObjectURL(u); }
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
  const nonce = SENDTAB + "-" + (++nonceCtr);
  if(deps.onBeforeSend) deps.onBeforeSend(created);

  const echo = text || ("📎 " + staged.map(f => f.name).join(", "));

  // clear the composer immediately
  if(ta){ ta.value = ""; autoGrow(ta); }
  files = []; renderChips();
  deps.rerender(created);
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
    if(!r.ok){ rollback(deps, created, nonce, text, staged, (await r.text()).trim() || "сообщение не принято"); return; }
    const data = await r.json();
    const blobs = [];
    const thumbs = staged.filter(f => /^image\//.test(f.file.type)).map(f => { const u = URL.createObjectURL(f.file); blobs.push(u); return "![](" + u + ")"; });
    if(blobs.length) blobsByNonce.set(nonce, blobs);
    deps.model.upsertUser(created, { seq: data.seq, nonce, text: [echo, ...thumbs].join("\n\n"), time: Date.now() }, data.state || "enq");
    deps.rerender(created);
  } catch(e){
    rollback(deps, created, nonce, text, staged, "сеть недоступна — сообщение не отправлено");
  }
}

function rollback(deps, created, nonce, text, staged, msg){
  (blobsByNonce.get(nonce) || []).forEach(u => URL.revokeObjectURL(u));
  blobsByNonce.delete(nonce);
  deps.model.rollback(created, nonce);
  deps.myNonces.delete(nonce);
  // Restore the composed text/files so a failed send doesn't silently vanish — but only if
  // the composer hasn't already been reused for a newer draft.
  const ta = document.getElementById("input");
  if(ta && !ta.value.trim() && !files.length){ ta.value = text || ""; autoGrow(ta); files = (staged || []).slice(); renderChips(); }
  if(msg && deps.notice) deps.notice(msg);
  deps.rerender(created);
}
