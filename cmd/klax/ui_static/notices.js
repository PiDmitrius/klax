import { copyText, flashCopied } from "./base.js";

export function noticeSeverity(text, requested){
  if(requested === "error" || requested === "warning" || requested === "info") return requested;
  if(requested && requested.error) return "error";
  if(requested && requested.warning) return "warning";
  const s = String(text || "").toLowerCase();
  if(/[❌⛔]/.test(s) || /ошиб|не удалось|недоступ|не отправ|не принят|отменен|отменён|потер|failed|unauthorized|forbidden/.test(s)) return "error";
  if(/[⚠⏳🔄]/.test(s) || /перезапуск|перезапуска|занят|подожд|дождитесь|попробуйте|не восстановлено/.test(s)) return "warning";
  return "info";
}

export function noticeText(text){
  return String(text || "").replace(/^\s*(?:✅|❌|⛔|⚠️?|⏳|🔄)\s*/u, "");
}

const severityIcon = { info: "ⓘ", warning: "⚠", error: "✕" };

export function dismissNotice(el, immediate){
  if(!el || el._dismissing) return;
  el._dismissing = true;
  if(el._timer) clearTimeout(el._timer);
  if(immediate){ el.remove(); return; }
  el.classList.add("fade-out");
  el._fadeTimer = setTimeout(() => {
    el.style.height = el.offsetHeight + "px";
    void el.offsetHeight;
    el.classList.add("collapse");
    el._collapseTimer = setTimeout(() => el.remove(), 300);
  }, 350);
}

export function showNotice(text, opts){
  const container = document.getElementById("notifications");
  if(!container || !text) return;
  const severity = noticeSeverity(text, opts);
  const cleanText = noticeText(text);
  const el = document.createElement("div");
  el.className = "notify " + severity;
  el.setAttribute("data-severity", severity);
  const icon = document.createElement("span");
  icon.className = "notify-icon"; icon.setAttribute("aria-hidden", "true"); icon.textContent = severityIcon[severity];
  const body = document.createElement("span");
  body.className = "notify-text"; body.textContent = cleanText;
  const copy = document.createElement("button");
  copy.className = "notify-copy block-copy"; copy.title = "Копировать уведомление"; copy.setAttribute("aria-label", "Копировать уведомление"); copy.textContent = "⧉";
  copy.onclick = e => { e.stopPropagation(); copyText(cleanText, () => flashCopied(el)); };
  el.append(icon, body, copy);
  el.onclick = () => dismissNotice(el);
  container.appendChild(el);
  void el.offsetHeight;
  el.classList.add("visible");
  el._timer = setTimeout(() => dismissNotice(el), severity === "error" ? 7000 : severity === "warning" ? 6000 : 5000);
  const live = container.querySelectorAll(".notify:not(.fade-out)");
  for(let i = 0; i < live.length - 10; i++) dismissNotice(live[i], true);
}
