// Local visual harness. Activated only by an explicit ?debug query parameter;
// it has no server API, persistence, or external side effects.
export function initDebug({ notice }){
  if(typeof location === "undefined" || !new URLSearchParams(location.search).has("debug")) return;
  const panel = document.createElement("div"); panel.id = "debugpanel";
  const head = document.createElement("div"); head.className = "debug-head"; head.textContent = "Notifications";
  const close = document.createElement("button"); close.className = "debug-close"; close.title = "Закрыть"; close.textContent = "✕"; close.onclick = () => panel.remove(); head.appendChild(close);
  const actions = document.createElement("div"); actions.className = "debug-actions";
  const add = (label, run) => { const b = document.createElement("button"); b.textContent = label; b.onclick = run; actions.appendChild(b); };
  add("Info", () => notice("Информационное системное сообщение", "info"));
  add("Warning", () => notice("Требуется внимание пользователя", "warning"));
  add("Error", () => notice("Сообщение не принято", "error"));
  add("Многострочная", () => notice("Ошибка проверки обновлений\nПодробное описание ошибки на второй строке", "error"));
  add("Стек ×12", () => { for(let i = 1; i <= 12; i++) notice("Тестовое уведомление " + i, i % 3 === 0 ? "warning" : "info"); });
  panel.append(head, actions); document.body.appendChild(panel);
}
