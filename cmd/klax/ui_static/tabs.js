// tabs.js — the tab strip, /api/sessions reconcile, new/close, and the per-session
// settings modal (engine/model/effort/sandbox/tty/cwd/prompt + context gauge), ported
// from the monolith. deps: { select(created), onNew(created), afterClose(created),
// notice(text) }.

import { api } from "./base.js";
import { esc } from "./markdown.js";
import { uiConfirm } from "./modal.js";

let sessions = [], deps = {}, settingsFor = 0, settingsIsNew = false, settingsAutofocused = false;
// The shell's <title> (product name, server-injected) — the base for the unread prefix.
const BASE_TITLE = (typeof document !== "undefined" && document.title) || "klax";
function sameSession(a, b){ return String(a) === String(b); }

export function initTabs(d){
  deps = d;
  const nb = document.getElementById("newtab");
  if(nb) nb.addEventListener("click", newSession);
  const sc = document.getElementById("sclose");
  if(sc) sc.addEventListener("click", closeSettings);
  const sok = document.querySelector(".smodal-ok");
  if(sok) sok.addEventListener("click", closeSettings);
  const sm = document.getElementById("smodal");
  if(sm) sm.addEventListener("click", e => { if(e.target === sm) closeSettings(); }); // backdrop closes
  document.addEventListener("click", () => closeAllSelects()); // any click outside a menu closes it (menus stopPropagation their own)
  document.addEventListener("keydown", e => {
    if(e.key !== "Escape" || !settingsFor) return;
    const cm = document.getElementById("modal");
    if(cm && !cm.classList.contains("hidden")) return; // the confirm dialog owns Escape while open
    if(document.querySelector(".sselect.open")){ closeAllSelects(); return; } // Escape closes an open dropdown first
    closeSettings();
  });
}

export function reconcileSessions(list, active){ sessions = list || []; renderTabs(active); maybeRefreshSettings(); }

export function renderTabs(active){
  const strip = document.getElementById("tabs");
  if(!strip) return;
  let totalUnread = 0, busyCount = 0;
  // The badge AND the browser <title> mirror the in-log truth symmetrically: every tab
  // (active included) shows its REAL remaining unread (line-to-bottom), counting down as the
  // reader scrolls and hitting 0 exactly when the divider collapses. The title just sums the
  // same per-tab counts, so title and tabs never disagree — and no instant reset on entry.
  const existing = new Map();
  strip.querySelectorAll(".tab[data-created]").forEach(t => existing.set(t.dataset.created, t));
  const keep = new Set();
  for(const s of sessions){
    const unread = deps.unread ? deps.unread(s.created) : 0;
    const isActive = sameSession(s.created, active);
    totalUnread += unread;
    if(s.busy) busyCount++;
    const key = String(s.created);
    const t = existing.get(key) || createTab();
    keep.add(key);
    t.dataset.created = key;
    t._sessionName = s.name || "";
    t.className = "tab" + (isActive ? " active" : "") + (s.busy ? " busy" : "") + (unread ? " unread" : "");
    t.querySelector(".tname").textContent = s.name || ("сессия " + s.created);
    const badge = t.querySelector(".badge");
    badge.textContent = unread || "";
    badge.classList.toggle("hidden", !unread);
    if(t.parentNode !== strip || strip.children[sessions.indexOf(s)] !== t) strip.appendChild(t);
  }
  for(const [key, t] of existing) if(!keep.has(key)) t.remove();
  const mark = (totalUnread || "") + "*".repeat(busyCount); // unread count + one * per busy session
  document.title = mark ? "(" + mark + ") " + BASE_TITLE : BASE_TITLE;
}

function createTab(){
  const t = document.createElement("div");
  t.className = "tab";
  t.innerHTML = '<span class="dot"></span><span class="tname"></span><span class="badge hidden"></span><span class="tx" title="Закрыть">✕</span>';
  t.addEventListener("click", e => {
    if(e.target.classList.contains("tx")) return;
    const created = parseInt(t.dataset.created, 10);
    if(created && deps.select) deps.select(created);
  });
  t.addEventListener("dblclick", e => {
    if(e.target.classList.contains("tx")) return;
    e.preventDefault();
    const created = parseInt(t.dataset.created, 10);
    if(created) openSettings(created, "Настройки сессии", false);
  }); // settings via double-click (no per-tab gear)
  t.querySelector(".tx").addEventListener("click", e => {
    e.stopPropagation();
    const created = parseInt(t.dataset.created, 10);
    if(created) closeSession(created, t._sessionName);
  });
  return t;
}

function notice(t){ if(deps.notice) deps.notice(t); }

async function newSession(){
  try {
    const r = await api("/api/new", { method: "POST" });
    if(!r.ok){ notice("не удалось создать сессию"); return; }
    const d = await r.json();
    if(d.created){ if(deps.onNew) await deps.onNew(d.created); openSettings(d.created, "Новая сессия", true); }
  } catch(e){ notice("не удалось создать сессию"); }
}

async function closeSession(created, name){
  if(!(await uiConfirm("Закрыть сессию «" + (name || ("#" + created)) + "»?", "Закрыть", true))) return;
  try {
    const r = await api("/api/close", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: created }) });
    if(!r.ok){ notice((await r.text()).trim() || "не удалось закрыть"); return; }
    if(deps.afterClose) deps.afterClose(created);
  } catch(e){ notice("не удалось закрыть"); }
}

// --- settings modal ---
function fetchSettings(created){ return api("/api/settings?session=" + created).then(r => r.ok ? r.json() : Promise.reject(r)); }
function ctxClass(pct){ return pct >= 90 ? "crit" : pct >= 70 ? "hot" : ""; } // matches .sctx-fill.hot/.crit

// Custom dropdown (no native <select>, which renders in the OS style and whose open state we cannot
// see): a styled button + an absolutely-positioned menu. "Open" is our own class, so a background
// refresh can honestly hold off while a menu is open instead of yanking it (see maybeRefreshSettings).
function selectHTML(id, list, cur, withDefault, disabled){
  const opts = (withDefault ? [{ value: "", label: "По умолчанию" }] : []).concat(list || []);
  const curOpt = opts.find(o => o.value === cur);
  const curLabel = curOpt ? curOpt.label : (cur || "—");
  const menu = opts.map(o => '<div class="sselect-opt'+(o.value === cur ? " sel" : "")+'" data-value="'+esc(o.value)+'">'+esc(o.label)+'</div>').join("");
  return '<div class="sselect'+(disabled ? " disabled" : "")+'" id="'+id+'" data-value="'+esc(cur)+'">'
    +'<button type="button" class="sselect-btn"'+(disabled ? " disabled" : "")+'><span class="sselect-cur">'+esc(curLabel)+'</span><span class="sselect-caret">▾</span></button>'
    +'<div class="sselect-menu hidden">'+menu+'</div></div>';
}
function closeAllSelects(except){
  document.querySelectorAll(".sselect.open").forEach(s => {
    if(s === except) return;
    s.classList.remove("open");
    const m = s.querySelector(".sselect-menu"); if(m) m.classList.add("hidden");
  });
}
function wireSelect(id, onPick){
  const root = document.getElementById(id);
  if(!root || root.classList.contains("disabled")) return;
  const btn = root.querySelector(".sselect-btn"), menu = root.querySelector(".sselect-menu");
  btn.addEventListener("click", e => {
    e.stopPropagation();
    const willOpen = menu.classList.contains("hidden");
    closeAllSelects();
    if(willOpen){ menu.classList.remove("hidden"); root.classList.add("open"); }
  });
  menu.addEventListener("click", e => e.stopPropagation()); // a click on the menu chrome (padding/scrollbar) must not close it
  menu.querySelectorAll(".sselect-opt").forEach(opt => opt.addEventListener("click", e => {
    e.stopPropagation();
    menu.classList.add("hidden"); root.classList.remove("open");
    const v = opt.dataset.value;
    if(v !== root.dataset.value) onPick(v);
  }));
}

export function openSettings(created, title, isNew){
  settingsFor = created; settingsIsNew = !!isNew; settingsAutofocused = false;
  const tt = document.querySelector(".smodal-title"); if(tt) tt.textContent = title || "Настройки сессии";
  document.getElementById("smodal").classList.remove("hidden");
  document.getElementById("sbody").innerHTML = '<div class="shint">Загрузка…</div>';
  fetchSettings(created).then(d => { if(settingsFor === created) renderSettings(d); })
    .catch(() => { if(settingsFor === created) document.getElementById("sbody").innerHTML = '<div class="shint">Не удалось загрузить настройки</div>'; });
}
function closeSettings(){
  settingsFor = 0; settingsIsNew = false; settingsAutofocused = false;
  document.getElementById("smodal").classList.add("hidden");
  // Dismissing settings (notably the "Новая сессия" dialog that auto-opens on create and
  // grabs the name field) hands focus back to the composer so you can type straight away.
  if(deps.focus) deps.focus();
}

function renderSettings(d){
  if(settingsFor !== d.created) return;
  const lock = d.busy, dis = lock ? " disabled" : "";
  let h = "";
  if(lock) h += '<div class="sbusy">⏳ Сессия занята — параметры запуска нельзя менять до завершения.</div>';
  h += '<div class="srow"><label>Имя</label><div class="sctl"><input class="sname" type="text" maxlength="80" value="'+esc(d.name)+'"></div></div>';
  h += '<div class="srow"><label>Движок</label><div class="sctl">'+selectHTML("s-backend", d.backends, d.backend, false, !!(d.backend_locked || lock))+'</div></div>';
  if(d.backend_locked) h += '<div class="shint">Движок зафиксирован после первого сообщения.</div>';
  h += '<div class="srow"><label>Модель</label><div class="sctl">'+selectHTML("s-model", d.models, d.model, true, !!lock)+'</div></div>';
  h += '<div class="srow"><label>Мышление</label><div class="sctl">'+selectHTML("s-think", d.efforts, d.think, true, !!lock)+'</div></div>';
  h += '<div class="srow"><label>Sandbox</label><div class="sctl"><label class="stoggle"><input type="checkbox" id="s-sandbox"'+(d.sandbox==="on"?" checked":"")+dis+'><span>'+(d.sandbox==="on"?"вкл":"выкл")+'</span></label></div></div>';
  if(d.tty_available)
    h += '<div class="srow"><label>TTY</label><div class="sctl"><label class="stoggle"><input type="checkbox" id="s-tty"'+(d.tty?" checked":"")+dis+'><span>'+(d.tty?"вкл":"выкл")+'</span></label></div></div>';
  h += '<div class="sfield"><label>Рабочий каталог</label><input class="scwd" type="text"'+dis+' value="'+esc(d.cwd||"")+'"></div>';
  h += '<div class="sfield"><label>Системный промпт</label><textarea class="sprompt" rows="2"'+dis+' placeholder="добавляется к системному промпту">'+esc(d.prompt||"")+'</textarea></div>';
  h += '<div class="ssep"></div>';
  if(d.ctx_window > 0){
    const pct = Math.min(100, Math.round(d.ctx_used*100/d.ctx_window));
    h += '<div class="sctx-label"><span>Контекст</span><span>'+Math.round(d.ctx_used/1000)+'k / '+Math.round(d.ctx_window/1000)+'k · '+pct+'%</span></div>'
       + '<div class="sctx-bar"><div class="sctx-fill '+ctxClass(pct)+'" style="width:'+pct+'%"></div></div>';
  } else h += '<div class="sctx-label"><span>Контекст</span><span>—</span></div>';
  h += '<div class="shint" style="margin-top:8px">💬 Сообщений: '+(d.messages||0)+'</div>';
  const b = document.getElementById("sbody");
  b.innerHTML = h;

  const nameInput = b.querySelector(".sname");
  const applyName = () => { const v = nameInput.value.trim(); if(v && v !== d.name) patchSettings(d.created, { name: v }); };
  nameInput.addEventListener("keydown", e => { if(e.key !== "Enter") return; e.preventDefault(); nameInput.blur(); if(settingsIsNew) closeSettings(); });
  nameInput.addEventListener("blur", applyName);
  if(settingsIsNew && settingsFor === d.created && !settingsAutofocused){
    settingsAutofocused = true;
    nameInput.focus();
    nameInput.select();
  }
  wireSelect("s-backend", v => patchSettings(d.created, { backend: v }));
  wireSelect("s-model",   v => patchSettings(d.created, { model: v }));
  wireSelect("s-think",   v => patchSettings(d.created, { think: v }));
  const wire = (sel, fn) => { const el = b.querySelector(sel); if(el) el.onchange = fn; };
  wire("#s-sandbox", e => patchSettings(d.created, { sandbox: e.target.checked ? "on" : "off" }));
  wire("#s-tty",     e => patchSettings(d.created, { tty: e.target.checked }));
  const cwd = b.querySelector(".scwd");
  if(cwd){
    const applyCwd = () => { const v = cwd.value.trim(); if(v && v !== d.cwd) patchSettings(d.created, { cwd: v }); };
    cwd.addEventListener("keydown", e => { if(e.key === "Enter"){ e.preventDefault(); cwd.blur(); } });
    cwd.addEventListener("blur", applyCwd);
  }
  const prompt = b.querySelector(".sprompt");
  if(prompt) prompt.addEventListener("blur", () => { const v = prompt.value.trim(); if(v !== (d.prompt||"")) patchSettings(d.created, { prompt: v }); });
}

function patchSettings(created, patch){
  patch.session = created;
  api("/api/settings", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(patch) })
    .then(r => { if(r.ok) return r.json(); r.text().then(t => notice(t.trim() || "не удалось применить")); return fetchSettings(created); })
    .then(d => { if(d && settingsFor === created) renderSettings(d); })
    .catch(() => notice("не удалось применить"));
}

// maybeRefreshSettings re-syncs an open dialog when sessions change (busy lock/unlock, ctx update).
// It must NOT clobber a text field mid-edit — those hold unsaved keystrokes that a re-render would
// drop — so it skips while a text input / textarea in the body has focus. Selects and checkboxes
// apply immediately on change (nothing unsaved), so a busy/context refresh may proceed even while one
// is focused; we just restore focus to that same control afterward so it isn't yanked out. Otherwise
// the dialog would stay stale (banner/disabled state) for as long as a select kept focus.
function maybeRefreshSettings(){
  if(!settingsFor) return;
  if(document.querySelector(".sselect.open")) return; // a dropdown is open — hold the refresh, don't yank the menu
  const ae = document.activeElement, body = document.getElementById("sbody");
  const inBody = !!(ae && body && body.contains(ae));
  if(inBody && (ae.tagName === "TEXTAREA" || (ae.tagName === "INPUT" && (ae.type || "text") === "text"))) return;
  const keepId = inBody ? ae.id : "";
  fetchSettings(settingsFor).then(d => {
    if(settingsFor !== d.created) return;
    const refocus = keepId && document.activeElement && document.activeElement.id === keepId; // still there after the async fetch
    renderSettings(d);
    if(refocus){ const el = document.getElementById(keepId); if(el && !el.disabled) el.focus(); }
  }).catch(()=>{});
}
