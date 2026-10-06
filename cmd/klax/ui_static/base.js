// base.js — mount-aware URL helpers, auth token, and the authenticated fetch wrapper.
// Lazily touches location/localStorage (only when first called) so the module graph can
// be imported under plain node for unit tests without a DOM.

const TOKEN_KEY = "klax_ui_token";
let _base, _token, authFailure = () => {};
export function onAuthFailure(fn){ authFailure = fn; }

// BASE is the path the SPA is served under (with a trailing slash) — "/" normally,
// "/klax/" behind a path-stripping reverse proxy.
export function BASE(){
  return _base ?? (_base = location.pathname.endsWith("/") ? location.pathname : location.pathname + "/");
}

export function getToken(){
  if(_token === undefined || _token === null){ try { _token = localStorage.getItem(TOKEN_KEY) || ""; } catch(_){ _token = ""; } }
  return _token;
}
export function setToken(t){
  _token = t;
  try { if(t) localStorage.setItem(TOKEN_KEY, t); else localStorage.removeItem(TOKEN_KEY); } catch(_){}
}

// Canonical input-modality capability used by composer and focus management. Keep this as a
// capability check, not a user-agent/device-name branch: hybrid devices may also have a mouse.
export function hasCoarsePointer(){
  return typeof matchMedia === "function" && matchMedia("(pointer: coarse)").matches;
}

// Touch activation precedes blur-driven layout changes and consumes the matching click.
export function bindButtonActivation(button, activate){
  let touchActivated = false, touchTimer = 0;
  button.addEventListener("pointerdown", e => {
    if(e.pointerType !== "touch") return;
    e.preventDefault();
    touchActivated = true;
    clearTimeout(touchTimer);
    touchTimer = setTimeout(() => { touchActivated = false; }, 1200);
    activate(true);
  });
  button.addEventListener("click", () => {
    if(touchActivated){ touchActivated = false; clearTimeout(touchTimer); return; }
    activate(false);
  });
}

// apiHref prefixes our own root-absolute /api/... URLs with BASE so they resolve behind
// the mount proxy; remote (http/https) URLs pass through untouched.
export function apiHref(href){ return href.charAt(0) === "/" ? BASE() + href.slice(1) : href; }

let requestPolicy;
function policy(){ return requestPolicy ||= JSON.parse(document.getElementById("request-policy").textContent); }
export function retryDelay(attempt){
  const p = policy();
  let delay = p.retry_min_ms;
  for(let i = 0; i < attempt && delay < p.retry_max_ms; i++) delay *= 2;
  return Math.min(delay, p.retry_max_ms);
}

// api bounds the authenticated request through receipt of the complete body.
export async function api(path, opts = {}, longPoll = false){
  const token = getToken(), ac = new AbortController();
  const abort = () => ac.abort(opts.signal.reason);
  if(opts.signal){
    if(opts.signal.aborted) abort();
    else opts.signal.addEventListener("abort", abort, { once: true });
  }
  const timer = setTimeout(() => ac.abort(new DOMException("Request timed out", "TimeoutError")), longPoll ? policy().poll_ms : policy().request_ms);
  try {
    if(ac.signal.aborted) throw ac.signal.reason;
    const r = await fetch(BASE() + (path[0] === "/" ? path.slice(1) : path), {
      ...opts, signal: ac.signal,
      headers: Object.assign({ "Authorization": "Bearer " + token }, opts.headers || {}),
    });
    if(r.status === 401 && token === getToken()) authFailure();
    const body = await r.arrayBuffer();
    return new Response([204, 205, 304].includes(r.status) ? null : body, { status: r.status, statusText: r.statusText, headers: r.headers });
  } finally {
    clearTimeout(timer);
    if(opts.signal) opts.signal.removeEventListener("abort", abort);
  }
}

// home is the server's home directory (from /api/state): paths are absolute on the wire and shown
// abbreviated to ~ here.
let home = "";
export function setHome(h){ home = h || ""; }
export function tildePath(p){
  if(!home || !p) return p || "";
  if(p === home) return "~";
  return p.startsWith(home + "/") ? "~" + p.slice(home.length) : p;
}

// apiError is the message of an /api/* error response ({"error":{"code","message"}}), or fallback.
export async function apiError(r, fallback){
  try { const e = await r.json(); if(e && e.error && e.error.message) return e.error.message; } catch(_){}
  return fallback;
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
