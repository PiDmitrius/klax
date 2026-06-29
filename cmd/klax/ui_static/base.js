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

// apiHref prefixes our own root-absolute /api/... URLs with BASE so they resolve behind
// the mount proxy; remote (http/https) URLs pass through untouched.
export function apiHref(href){ return href.charAt(0) === "/" ? BASE() + href.slice(1) : href; }

// api is the authenticated fetch: Bearer token + BASE-relative path.
export function api(path, opts){
  opts = opts || {};
  opts.headers = Object.assign({ "Authorization": "Bearer " + getToken() }, opts.headers || {});
  return fetch(BASE() + (path[0] === "/" ? path.slice(1) : path), opts);
}
