// base.js — mount-aware URL helpers, auth token, and the authenticated fetch wrapper.
// Lazily touches location/localStorage (only when first called) so the module graph can
// be imported under plain node for unit tests without a DOM.

const TOKEN_KEY = "klax_ui_token";
let _base, _token;

// BASE is the path the SPA is served under (with a trailing slash) — "/" normally,
// "/klax/" behind a path-stripping reverse proxy.
export function BASE(){
  return _base ?? (_base = location.pathname.endsWith("/") ? location.pathname : location.pathname + "/");
}

export function getToken(){
  if(_token === undefined || _token === null) _token = localStorage.getItem(TOKEN_KEY) || "";
  return _token;
}
export function setToken(t){ _token = t; localStorage.setItem(TOKEN_KEY, t); }

// Canonical input-modality capability used by composer and focus management. Keep this as a
// capability check, not a user-agent/device-name branch: hybrid devices may also have a mouse.
export function hasCoarsePointer(){
  return typeof matchMedia === "function" && matchMedia("(pointer: coarse)").matches;
}

// apiHref prefixes our own root-absolute /api/... URLs with BASE so they resolve behind
// the mount proxy; remote (http/https) URLs pass through untouched.
export function apiHref(href){ return href.charAt(0) === "/" ? BASE() + href.slice(1) : href; }

// api is the authenticated fetch: Bearer token + BASE-relative path.
export function api(path, opts){
  opts = opts || {};
  opts.headers = Object.assign({ "Authorization": "Bearer " + getToken() }, opts.headers || {});
  return fetch(BASE() + (path[0] === "/" ? path.slice(1) : path), opts);
}

// --- click-to-copy: ONE implementation shared by every copyable surface (timeline code,
// message body, the session UUID) so the copy behaviour AND its flash look identical everywhere.

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
export function copyText(text, ok){
  if(navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(ok).catch(() => fallbackCopy(text, ok));
  else fallbackCopy(text, ok);
}
// flashCopied replays the shared .copyflash animation on the copied element.
export function flashCopied(el){
  if(!el) return;
  el.classList.remove("copyflash");
  void el.offsetWidth;
  el.classList.add("copyflash");
  el.addEventListener("animationend", () => el.classList.remove("copyflash"), { once: true });
}
