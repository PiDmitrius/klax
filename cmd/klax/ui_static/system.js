import { api, copyText, flashCopied } from "./base.js";
import { uiConfirm } from "./modal.js";

const $ = id => document.getElementById(id);
let refreshTimer = 0;
let notify = () => {};
let lastData = null;
let seenCheckError = "";

function codeValue(value){
  const el = document.createElement("code"); el.className = "syscode"; el.textContent = value || "—";
  el.title = "Копировать";
  el.onclick = () => copyText(value || "", () => flashCopied(el));
  return el;
}

function row(label, value, opts){
  opts = opts || {};
  const el = document.createElement("div"); el.className = "sysrow";
  const k = document.createElement("span"); k.className = "syskey"; k.textContent = label;
  const group = document.createElement("span"); group.className = "sysvalgroup";
  let v = opts.copy ? codeValue(value) : opts.link ? document.createElement("a") : document.createElement("span");
  if(!opts.copy){ v.className = "sysval"; v.textContent = value || "—"; }
  if(opts.link){ v.href = opts.link; v.target = "_blank"; v.rel = "noopener noreferrer"; }
  if(!opts.noValue) group.appendChild(v);
  if(opts.button) group.appendChild(opts.button);
  el.append(k, group); return el;
}

function elapsed(sec){
  sec = Math.max(0, Number(sec) || 0);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return (d ? d + " д " : "") + (h ? h + " ч " : "") + m + " мин";
}

function render(data){
  lastData = data;
  const body = $("sysbody"), u = data.update || {};
  body.textContent = "";
  body.append(row("Версия", "v" + data.version, { copy: true }), row("Запущен", new Date(data.started_at).toLocaleString()), row("Работает", elapsed(data.uptime_sec)), row("Процесс", String(data.pid), { copy: true }), row("Платформа", data.platform));
  body.appendChild(Object.assign(document.createElement("div"), { className: "syssep" }));
  if(u.source_dir) body.append(row("Исходник", u.source_dir, { copy: true }));
  const check = document.createElement("button"); check.id = "syscheck"; check.className = "syscheck";
  check.disabled = !!u.checking; check.textContent = u.checking ? "Проверяется…" : "Проверить"; check.onclick = checkUpdates;
  body.append(row("Обновления", "", { noValue: true, button: check }));
  if((u.releases || []).length){
    const list = document.createElement("div"); list.className = "sysreleases";
    for(const release of (u.releases || [])){
      const item = document.createElement("div"); item.className = "sysrelease";
      const bullet = document.createElement("span"); bullet.className = "sysbullet"; bullet.setAttribute("aria-hidden", "true"); bullet.textContent = "•";
      const age = release.url ? document.createElement("a") : document.createElement("span"); age.className = "sysage"; age.textContent = release.age || "";
      if(release.url){ age.href = release.url; age.target = "_blank"; age.rel = "noopener noreferrer"; }
      const action = document.createElement("button"); action.className = "sysaction" + (release.action === "update" ? " update" : "");
      action.dataset.tag = release.tag; action.dataset.action = release.action; action.disabled = !!u.running;
      action.dataset.source = release.source;
      action.textContent = actionLabel(release.action);
      action.onclick = installFound;
      item.append(bullet, codeValue(release.tag), age, action); list.appendChild(item);
    }
    body.appendChild(list);
  } else if(u.checked && !u.checking){
    const empty = document.createElement("div"); empty.className = "sysrelease-empty"; empty.textContent = "Релизы не найдены"; body.appendChild(empty);
  }
  if(u.check_error && u.check_error !== seenCheckError){
    seenCheckError = u.check_error;
    notify("Ошибка проверки обновлений\n" + u.check_error, { error: true });
  }
  if(!u.check_error) seenCheckError = "";
  clearTimeout(refreshTimer);
  if(u.running || (!$("sysmodal").classList.contains("hidden") && u.checking)) refreshTimer = setTimeout(refresh, 1200);
}

async function refresh(){
  try {
    const r = await api("/api/system");
    if(!r.ok) throw new Error(await r.text());
    render(await r.json());
  } catch(e){ $("sysbody").textContent = "Не удалось получить состояние klax"; }
}

function close(){ clearTimeout(refreshTimer); $("sysmodal").classList.add("hidden"); }

function actionLabel(action){ return ({ update: "Обновить", install: "Установить", reinstall: "Переустановить" }[action] || "Установить"); }

function confirmText(pending){
  if(pending.action === "update") return "Обновить klax " + pending.current + " до " + pending.tag + "?";
  if(pending.action === "reinstall") return "Переустановить klax " + pending.tag + "?";
  return "Установить klax " + pending.tag + " вместо " + pending.current + "?";
}

function errorNotice(title, error){
  const detail = String(error && error.message || "").trim();
  return detail ? title + "\n" + detail : title;
}

async function checkUpdates(){
  const b = $("syscheck"); if(b){ b.disabled = true; b.textContent = "Проверяется…"; }
  try {
    const r = await api("/api/system/check", { method: "POST" });
    if(!r.ok) throw new Error(await r.text());
    refresh();
  } catch(e){ notify(errorNotice("Ошибка проверки обновлений", e), { error: true }); refresh(); }
}

async function installFound(event){
  const button = event.currentTarget;
  const chosen = { tag: button.dataset.tag, source: button.dataset.source, action: button.dataset.action, current: "v" + lastData.version };
  if(!(await uiConfirm(confirmText(chosen), actionLabel(chosen.action)))) return;
  close();
  beginInstall(chosen);
}

async function beginInstall(chosen){
  try {
    const r = await api("/api/system/update", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ tag: chosen.tag, source: chosen.source }) });
    const data = await r.json();
    if(!r.ok) throw new Error(data.message || "Ошибка установки");
    notify(data.message, data.started ? "info" : "warning");
    if(lastData && lastData.update){ lastData.update.running = !!data.running; render(lastData); }
    refresh();
  } catch(e){ notify(errorNotice("Ошибка установки", e), { error: true }); refresh(); }
}

export function systemRestartNotice(kind, runningVersion){
  close();
  const tag = String(runningVersion || "").replace(/^v?/, "v");
  return (kind === "installed" ? "Установлен klax " : "Запущен klax ") + tag;
}

export function initSystem({ notice }){
  notify = notice || (() => {});
  $("sysbtn").onclick = () => { $("sysmodal").classList.remove("hidden"); $("sysbody").textContent = "Загрузка…"; refresh(); };
  $("sysclose").onclick = close; $("sysok").onclick = close;
  $("sysmodal").onclick = e => { if(e.target === $("sysmodal")) close(); };
  document.addEventListener("keydown", e => { if(e.key === "Escape" && !$("sysmodal").classList.contains("hidden")) close(); });
}
