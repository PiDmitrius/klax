// tabs.js — the tab strip, /api/sessions reconcile, new/close, and the per-session
// settings modal (engine/model/effort/sandbox/tty/cwd/prompt + context gauge), ported
// from the monolith. deps: { select(created), onNew(created), afterClose(created),
// notice(text) }.

import { api, copyText, flashCopied } from "./base.js";
import { esc } from "./markdown.js";
import { uiConfirm } from "./modal.js";

let sessions = [], deps = {}, settingsFor = 0, settingsAutofocused = false;
// draft (non-null) = the "new session" dialog is open for a session that does NOT exist yet.
// The "+" no longer creates immediately: it opens this draft, and the session is born only on
// OK/Enter. Closing the dialog (✕ / backdrop / Escape) discards the draft and creates nothing.
// draftView caches the last server option-lists (models/efforts for the chosen backend).
let draft = null, draftView = null, draftSubmitting = false;
// dragging = a tab reorder drag/settle is in progress; while true renderTabs leaves the strip DOM
// alone. didDrag = the click immediately after a drop must be swallowed (not treated as a select).
let dragging = false, didDrag = false;
const DRAG_SETTLE_MS = 180;
// The shell's <title> (product name, server-injected) — the base for the unread prefix.
const BASE_TITLE = (typeof document !== "undefined" && document.title) || "klax";
function sameSession(a, b){ return String(a) === String(b); }

export function initTabs(d){
  deps = d;
  const nb = document.getElementById("newtab");
  if(nb) nb.addEventListener("click", openDraft);
  const sc = document.getElementById("sclose");
  if(sc) sc.addEventListener("click", closeSettings);
  const sok = document.querySelector(".smodal-ok");
  if(sok) sok.addEventListener("click", onModalOk);
  const sm = document.getElementById("smodal");
  if(sm){
    // Backdrop closes ONLY on a full click that both PRESSES and RELEASES on the backdrop. Tracking
    // the pointerdown target stops a text-selection drag that starts inside a field and happens to
    // release outside the box from being mistaken for a backdrop click (which closed the dialog).
    let downOnBackdrop = false;
    sm.addEventListener("pointerdown", e => { downOnBackdrop = e.target === sm; });
    sm.addEventListener("click", e => { if(e.target === sm && downOnBackdrop) closeSettings(); });
  }
  document.addEventListener("click", () => closeAllSelects()); // any click outside a menu closes it (menus stopPropagation their own)
  document.addEventListener("keydown", e => {
    if(e.key !== "Escape" || (!settingsFor && !draft)) return;
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
  if(dragging) return; // a reorder drag owns the strip DOM — don't reconcile under it (drop re-renders)
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
    if(didDrag){ didDrag = false; return; } // this click is the tail of a drop — don't also select
    if(e.target.classList.contains("tx")) return;
    const created = parseInt(t.dataset.created, 10);
    if(created && deps.select) deps.select(created);
  });
  t.addEventListener("dblclick", e => {
    if(e.target.classList.contains("tx")) return;
    e.preventDefault();
    const created = parseInt(t.dataset.created, 10);
    if(created) openSettings(created, "Настройки сессии");
  }); // settings via double-click (no per-tab gear)
  t.querySelector(".tx").addEventListener("click", e => {
    e.stopPropagation();
    const created = parseInt(t.dataset.created, 10);
    if(created) closeSession(created, t._sessionName);
  });
  t.addEventListener("pointerdown", e => {
    if(e.button !== 0 || e.target.classList.contains("tx")) return; // left-button, not the close ✕
    startDrag(e, t);
  });
  return t;
}

// startDrag runs an animated, live sortable reorder. Past a small threshold the grabbed tab LIFTS
// (scales up + shadow) and glues to the cursor while the OTHER tabs slide apart in real time to open
// its landing slot — so the final arrangement is always visible under the cursor, with no separate
// marker. On release the lifted tab settles into that slot (FLIP), the order is POSTed to
// /api/reorder, and the server broadcast reconciles the canonical order (one source of truth).
// Below the threshold nothing happens and it stays a plain click (select) / dblclick (settings).
function startDrag(e, tab){
  const strip = document.getElementById("tabs");
  if(!strip || strip.querySelectorAll(".tab[data-created]").length < 2) return; // nothing to reorder
  didDrag = false; // fresh gesture — clear any stale flag so it can't swallow this click
  const startX = e.clientX, startY = e.clientY;
  let active = false, gap = 0, foot = 0, halfW = 0, fromIdx = 0, origCenter = 0, otherEls = [], otherCenters = [], otherOrigIdx = [], toK = 0;

  const begin = () => {
    active = true; dragging = true;
    try { tab.setPointerCapture(e.pointerId); } catch(_){}
    document.body.classList.add("dragging-tab");
    const full = Array.from(strip.querySelectorAll(".tab[data-created]"));
    fromIdx = full.indexOf(tab);
    const gcs = getComputedStyle(strip);
    gap = parseFloat(gcs.columnGap || gcs.gap) || 0;
    const rect = tab.getBoundingClientRect();
    foot = rect.width + gap;                 // the footprint the dragged tab inserts/removes
    halfW = rect.width / 2;                    // its half-width — the 50%-overlap trigger distance
    origCenter = rect.left + rect.width / 2;  // its center in the ORIGINAL (transform-free) layout
    otherEls = []; otherCenters = []; otherOrigIdx = [];
    full.forEach((el, i) => {
      if(el === tab) return;
      const r = el.getBoundingClientRect();
      otherEls.push(el); otherCenters.push(r.left + r.width / 2); otherOrigIdx.push(i);
      el.style.transition = "transform " + DRAG_SETTLE_MS + "ms ease";
    });
    tab.classList.add("tabdrag"); // lift: z-index + shadow (scale comes from the inline transform)
  };

  // layout(dx) positions the lifted tab under the cursor and shifts the other tabs to open the slot it
  // hovers. The swap trigger is 50% OVERLAP, not "cursor past the neighbor's center": a neighbor slides
  // aside once the dragged tab's LEADING EDGE passes that neighbor's center — the same visual overlap
  // for every neighbor regardless of its width (a wide tab no longer needs you to drag across all of
  // it). The ±halfW bias shifts each neighbor's threshold to its near half; thresholds stay monotone
  // (the original slot is wider than the dragged tab), so this is a stable pure function of dx — no
  // oscillation. Only the dragged footprint moves, so each sibling shifts by 0 or ±foot.
  const layout = dx => {
    const center = origCenter + dx;
    let k = 0;
    for(let j = 0; j < otherCenters.length; j++){
      const trigger = otherCenters[j] + (j < fromIdx ? halfW : -halfW);
      if(center > trigger) k++; else break;
    }
    toK = k;
    tab.style.transform = "translateX(" + Math.round(dx) + "px) scale(1.05)";
    for(let j = 0; j < otherEls.length; j++){
      const finalIdx = j < k ? j : j + 1;      // where sibling j ends up once the tab lands at k
      const diff = finalIdx - otherOrigIdx[j];  // ∈ {-1,0,1} — it only ever crosses the dragged slot
      otherEls[j].style.transform = diff ? "translateX(" + (diff * foot) + "px)" : "";
    }
  };

  const onMove = ev => {
    if(!active){
      if(Math.abs(ev.clientX - startX) < 5 && Math.abs(ev.clientY - startY) < 5) return;
      begin();
    }
    layout(ev.clientX - startX);
    ev.preventDefault();
  };

  // finish(commit): pointerUP commits the reorder; pointerCANCEL (OS/scroll interruption, lost capture)
  // ABORTS it — restore the original layout, no DOM reorder, no POST. Only a real drop reorders.
  const finish = commit => {
    tab.removeEventListener("pointermove", onMove);
    tab.removeEventListener("pointerup", onUp);
    tab.removeEventListener("pointercancel", onCancel);
    if(!active) return; // never crossed the threshold — leave it as a click
    didDrag = true;     // swallow the trailing click so the gesture doesn't also select the tab
    if(!commit){
      // Cancelled: the DOM was never reordered during the drag (only transforms), so a plain cleanup
      // returns everything to its original place.
      otherEls.forEach(el => { el.style.transition = ""; el.style.transform = ""; });
      tab.classList.remove("tabdrag", "tabsettle");
      tab.style.transition = ""; tab.style.transform = "";
      document.body.classList.remove("dragging-tab");
      dragging = false;
      return;
    }
    // Final order = the other tabs with the dragged one inserted at its hovered slot (toK).
    const ordered = otherEls.slice(0, toK).concat(tab, otherEls.slice(toK));
    // FLIP settle: measure the lifted tab where it sits, commit the real DOM order + clear all
    // transforms (siblings already visually occupy their final slots, so clearing is jump-free),
    // then animate the tab from its lifted spot into the freshly-opened slot.
    const first = tab.getBoundingClientRect();
    otherEls.forEach(el => { el.style.transition = ""; el.style.transform = ""; });
    tab.style.transition = ""; tab.style.transform = "";
    ordered.forEach(el => strip.appendChild(el)); // reorder DOM to the final sequence
    const last = tab.getBoundingClientRect();
    tab.style.transform = "translateX(" + Math.round(first.left - last.left) + "px) scale(1.05)";
    requestAnimationFrame(() => {
      tab.classList.add("tabsettle"); // transitions transform + shadow back to rest
      requestAnimationFrame(() => { tab.style.transform = ""; });
    });
    let settled = false;
    const done = () => {
      if(settled) return; // fires from transitionend OR the fallback timeout, whichever first
      settled = true;
      tab.classList.remove("tabdrag", "tabsettle");
      tab.style.transform = "";
      document.body.classList.remove("dragging-tab");
      dragging = false;
      // NOTE: do NOT re-render here with a locally-guessed active — the strip's `.active` class was
      // never touched during the drag, so the client's viewed tab stays highlighted, and the DOM is
      // already in final order. The server's Active flag is NOT the client's viewed tab (there is no
      // /api/switch), so rendering from it would falsely light up a different tab. The reorder POST's
      // broadcast triggers app.js's own reconcile with the REAL active shortly after.
    };
    tab.addEventListener("transitionend", done, { once: true });
    setTimeout(done, DRAG_SETTLE_MS + 80); // fallback if transitionend doesn't fire (no visible change)
    const order = ordered.map(el => parseInt(el.dataset.created, 10)).filter(Boolean);
    // On failure the server's (unchanged) order reconciles back via the next broadcast; tell the user
    // why their reorder didn't stick, matching rename/close/settings error handling elsewhere.
    api("/api/reorder", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ order }) })
      .then(r => { if(!r.ok) notice("не удалось сохранить порядок вкладок"); })
      .catch(() => notice("не удалось сохранить порядок вкладок"));
  };
  const onUp = () => finish(true);
  const onCancel = () => finish(false);
  tab.addEventListener("pointermove", onMove);
  tab.addEventListener("pointerup", onUp);
  tab.addEventListener("pointercancel", onCancel);
}

function notice(t){ if(deps.notice) deps.notice(t); }

// fetchDraft loads the "new session" option-lists + default values from the server. `backend`
// (optional) previews a specific backend's model/effort lists while the dialog is open.
function fetchDraft(backend){
  return api("/api/settings?session=0" + (backend ? "&backend=" + encodeURIComponent(backend) : ""))
    .then(r => r.ok ? r.json() : Promise.reject(r));
}
// openDraft opens the deferred-creation dialog. No session exists yet — settingsFor stays 0 and
// `draft` holds the pending field values; nothing is created until onModalOk/createFromDraft.
function openDraft(){
  settingsFor = 0; settingsAutofocused = false;
  draft = {}; draftView = null; draftSubmitting = false;
  const tt = document.querySelector(".smodal-title"); if(tt) tt.textContent = "Новая сессия";
  const ok = document.querySelector(".smodal-ok"); if(ok){ ok.textContent = "Создать"; ok.disabled = false; }
  document.getElementById("smodal").classList.remove("hidden");
  document.getElementById("sbody").innerHTML = '<div class="shint">Загрузка…</div>';
  fetchDraft("").then(d => {
    if(!draft) return; // dialog was dismissed while loading
    draft = { name: "", backend: d.backend, model: d.model || "", think: d.think || "", sandbox: d.sandbox, tty: !!d.tty, cwd: d.cwd || "", prompt: d.prompt || "" };
    renderDraft(d);
  }).catch(() => { if(draft) document.getElementById("sbody").innerHTML = '<div class="shint">Не удалось загрузить настройки</div>'; });
}
// renderDraft paints the draft dialog by overlaying the pending `draft` values onto the cached
// server option-lists (draftView), then reusing the shared renderSettings in draft mode.
function renderDraft(view){
  if(view) draftView = view;
  if(!draftView || !draft) return;
  const d = Object.assign({}, draftView, {
    created: 0, busy: false, backend_locked: false,
    name: draft.name || "", backend: draft.backend, model: draft.model || "", think: draft.think || "",
    sandbox: draft.sandbox, tty: !!draft.tty, cwd: draft.cwd || "", prompt: draft.prompt || "",
    assigned_model: "", session_id: "", messages: 0, ctx_window: 0, ctx_used: 0,
  });
  renderSettings(d, true);
}
// draftApply records a single field change into the pending draft and re-renders. A backend
// switch additionally re-fetches that backend's model/effort lists and resets the now-invalid
// model/think choices (mirrors the server's backend-switch reset for a real session).
function draftApply(patch){
  if(!draft) return;
  Object.assign(draft, patch);
  if("backend" in patch){
    draft.model = ""; draft.think = "";
    if(patch.backend !== "claude") draft.tty = false;
    fetchDraft(patch.backend).then(d => { if(draft) renderDraft(d); }).catch(() => {});
    return;
  }
  renderDraft();
}
// onModalOk is the shared OK button: confirm-and-create for a draft, plain close for a real session.
function onModalOk(){ if(draft) createFromDraft(); else closeSettings(); }
// createFromDraft POSTs the pending draft to /api/new (creation happens HERE, not on "+"), then
// switches to the freshly-created session. model/think are sent explicitly so "По умолчанию" is
// honoured; name/cwd/prompt only when non-empty (empty keeps the server-seeded default).
async function createFromDraft(){
  if(!draft || draftSubmitting) return;
  draftSubmitting = true;
  const ok = document.querySelector(".smodal-ok"); if(ok) ok.disabled = true;
  const d = draft, trim = v => (v || "").trim();
  const body = { backend: d.backend, model: d.model || "", think: d.think || "", sandbox: d.sandbox, tty: !!d.tty };
  if(trim(d.name)) body.name = trim(d.name);
  if(trim(d.cwd)) body.cwd = trim(d.cwd);
  body.prompt = trim(d.prompt);
  // Keep `draft` intact until the server accepts: on a rejected draft (e.g. an inaccessible working
  // directory) the server creates nothing and returns the reason, so we surface THAT and leave the
  // dialog open with the user's values to fix — instead of a generic error and a lost draft.
  let r;
  try {
    r = await api("/api/new", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  } catch(e){ draftSubmitting = false; if(ok) ok.disabled = false; notice("не удалось создать сессию"); return; }
  if(!r.ok){ draftSubmitting = false; if(ok) ok.disabled = false; notice((await r.text()).trim() || "не удалось создать сессию"); return; }
  const j = await r.json();
  draft = null; draftView = null;
  draftSubmitting = false; if(ok) ok.disabled = false;
  document.getElementById("smodal").classList.add("hidden");
  settingsFor = 0; settingsAutofocused = false;
  if(j.created && deps.onNew) await deps.onNew(j.created);
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

export function openSettings(created, title){
  settingsFor = created; settingsAutofocused = false;
  draft = null; draftView = null; // opening a real session's settings supersedes any stale draft state
  const tt = document.querySelector(".smodal-title"); if(tt) tt.textContent = title || "Настройки сессии";
  const ok = document.querySelector(".smodal-ok"); if(ok) ok.textContent = "OK";
  document.getElementById("smodal").classList.remove("hidden");
  document.getElementById("sbody").innerHTML = '<div class="shint">Загрузка…</div>';
  fetchSettings(created).then(d => { if(settingsFor === created) renderSettings(d); })
    .catch(() => { if(settingsFor === created) document.getElementById("sbody").innerHTML = '<div class="shint">Не удалось загрузить настройки</div>'; });
}
function closeSettings(){
  settingsFor = 0; settingsAutofocused = false;
  draft = null; draftView = null; draftSubmitting = false; // discard any pending "new session" draft — closing creates nothing
  const ok = document.querySelector(".smodal-ok"); if(ok) ok.disabled = false;
  document.getElementById("smodal").classList.add("hidden");
  // Dismissing settings (notably the "Новая сессия" dialog that auto-opens on create and
  // grabs the name field) hands focus back to the composer so you can type straight away.
  if(deps.focus) deps.focus();
}

function renderSettings(d, isDraft){
  if(isDraft){ if(!draft) return; } else if(settingsFor !== d.created) return;
  // In draft mode every control edits the pending `draft` object (nothing exists to PATCH yet);
  // for a real session each change applies immediately via patchSettings.
  const apply = isDraft ? draftApply : patch => patchSettings(d.created, patch);
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
  h += '<div class="sfield"><label>Системный промпт</label><textarea class="sprompt" rows="1"'+dis+' placeholder="добавляется к системному промпту">'+esc(d.prompt||"")+'</textarea></div>';
  // Read-only facts — the model the backend actually answered with (may differ from the selected
  // default) and the resolved session UUID. Only for a real session that has already answered (the
  // context gauge lives in the chat now, so it's no longer duplicated here).
  if(!isDraft && (d.assigned_model || d.session_id)){
    h += '<div class="ssep"></div>';
    if(d.assigned_model) h += '<div class="sfact"><span class="sfact-k">Модель</span><span class="sfact-v">'+esc(d.assigned_model)+'</span></div>';
    if(d.session_id) h += '<div class="sfact"><span class="sfact-k">UUID</span><code class="suuid" title="Скопировать">'+esc(d.session_id)+'</code></div>';
  }
  const b = document.getElementById("sbody");
  b.innerHTML = h;

  const nameInput = b.querySelector(".sname");
  const applyName = () => {
    const v = nameInput.value.trim();
    if(isDraft){ if(draft) draft.name = v; return; } // draft: hold locally, applied on create
    if(v && v !== d.name) patchSettings(d.created, { name: v });
  };
  nameInput.addEventListener("keydown", e => {
    if(e.key !== "Enter") return;
    e.preventDefault();
    nameInput.blur(); // commits the name (applyName)
    if(isDraft) createFromDraft(); else closeSettings(); // Enter = confirm/create the draft
  });
  nameInput.addEventListener("blur", applyName);
  // Grab the name field on every open (create AND double-click) so it can be edited and
  // committed with Enter straight away; the guard fires it once per open, not on each refresh.
  if((isDraft || settingsFor === d.created) && !settingsAutofocused){
    settingsAutofocused = true;
    nameInput.focus();
    nameInput.select();
  }
  wireSelect("s-backend", v => apply({ backend: v }));
  wireSelect("s-model",   v => apply({ model: v }));
  wireSelect("s-think",   v => apply({ think: v }));
  const wire = (sel, fn) => { const el = b.querySelector(sel); if(el) el.onchange = fn; };
  wire("#s-sandbox", e => apply({ sandbox: e.target.checked ? "on" : "off" }));
  wire("#s-tty",     e => apply({ tty: e.target.checked }));
  const cwd = b.querySelector(".scwd");
  if(cwd){
    const applyCwd = () => {
      const v = cwd.value.trim();
      if(isDraft){ if(draft) draft.cwd = v; return; }
      if(v && v !== d.cwd) patchSettings(d.created, { cwd: v });
    };
    cwd.addEventListener("keydown", e => { if(e.key === "Enter"){ e.preventDefault(); cwd.blur(); } });
    cwd.addEventListener("blur", applyCwd);
  }
  const prompt = b.querySelector(".sprompt");
  if(prompt) prompt.addEventListener("blur", () => {
    const v = prompt.value.trim();
    if(isDraft){ if(draft) draft.prompt = v; return; }
    if(v !== (d.prompt||"")) patchSettings(d.created, { prompt: v });
  });
  const uuid = b.querySelector(".suuid");
  if(uuid) uuid.addEventListener("click", () => copyText(uuid.textContent || "", () => flashCopied(uuid)));
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
