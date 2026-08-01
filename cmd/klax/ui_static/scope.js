// scope.js — the view scope: which subset of sessions this browser window shows.
//
// The scope lives in the URL fragment, so a scope is just a bookmarkable address and a second window
// is a plain browser tab — no new UI, no server routing. One namespace holds three things, told
// apart by shape (this parse rule is the whole contract):
//
//   #1783809783        all digits  → a session id, root scope
//   #work              has neither → a user group
//   #is:unread         has a colon → a computed view (parsed here, not implemented yet)
//   #work/1783809783   two segments: scope + the tab active inside it
//
// Everything scope-shaped is derived HERE and nowhere else: membership, the per-scope "last viewed
// tab" key, the hash writer, the window title prefix and the chip. Groups themselves are never stored
// by this module — they ride on the sessions.

import { esc } from "./markdown.js";

export const ROOT = { kind: "root", name: "" };

let scope = ROOT;
let menuOpen = false;

export function currentScope(){ return scope; }
export function isRoot(){ return scope.kind === "root"; }
export function scopeName(){ return scope.name; }
export function sameScope(a, b){ return a.kind === b.kind && a.name === b.name; }

// A group name is percent-encoded, except the colon that marks a computed view: it is legal in a
// fragment and `#is:unread` must stay readable.
function encodeScope(name){ return encodeURIComponent(name).replace(/%3A/gi, ":"); }

// parseHash reads the address bar. It never touches state, so callers can compare before/after.
export function parseHash(){
  const raw = (location.hash || "").replace(/^#/, "");
  if(!raw) return { scope: ROOT, created: 0 };
  const parts = raw.split("/");
  let first = "";
  try { first = decodeURIComponent(parts[0] || ""); } catch(e){ first = parts[0] || ""; }
  if(/^\d+$/.test(first)) return { scope: ROOT, created: parseInt(first, 10) || 0 };
  if(!first) return { scope: ROOT, created: 0 };
  let second = "";
  try { second = parts.length > 1 ? decodeURIComponent(parts[1]) : ""; } catch(e){ second = parts[1] || ""; }
  const created = /^\d+$/.test(second) ? parseInt(second, 10) : 0;
  return { scope: { kind: first.startsWith("is:") ? "builtin" : "group", name: first }, created };
}

export function setScope(s){ scope = s && s.kind ? s : ROOT; }

// inScope is the ONE membership test. Computed "is:*" views are deliberately empty for now: the URL
// parses and the empty state explains itself, rather than silently behaving like an unknown group.
export function inScope(sess){
  if(!sess) return false;
  if(scope.kind === "root") return true;
  if(scope.kind === "builtin") return false;
  return (sess.groups || []).indexOf(scope.name) !== -1;
}
export function filterScope(list){ return (list || []).filter(inScope); }

// Each scope remembers its own last-viewed tab, so a root window and a group window opened side by
// side do not fight over one key.
export function storageKey(){ return scope.kind === "root" ? "klax_active" : "klax_active:" + scope.kind + ":" + scope.name; }

// hashFor builds this scope's address for `created`. Root keeps the historical bare `#<created>`.
export function hashFor(created){
  const tab = created ? String(created) : "";
  if(scope.kind === "root") return tab;
  return encodeScope(scope.name) + (tab ? "/" + tab : "");
}

// writeHash keeps the address bar in step with the viewed tab WITHOUT pushing history: switching
// tabs is not navigation, and a push would make Back walk every tab you ever looked at. Scope
// changes are real navigation and go through ordinary links, which do push.
export function writeHash(created){
  const want = "#" + hashFor(created);
  if(location.hash === want || (!location.hash && want === "#")) return;
  try { history.replaceState(null, "", location.pathname + location.search + want); }
  catch(e){ location.hash = hashFor(created); }
}

// titlePrefix is the scope segment of the window title: "(3*) work — klax". The counter comes first
// because browsers truncate the tail, and `klax` is the droppable part.
export function titlePrefix(){ return scope.kind === "root" ? "" : scope.name + " — "; }

// knownGroups is the chat's group set, derived from the sessions — there is no registry, so a group
// exists exactly as long as a session carries it.
export function knownGroups(list){
  const seen = new Set();
  for(const s of list || []) for(const g of s.groups || []) seen.add(g);
  // This is the ONLY place the group set is derived — both the scope menu and the settings dialog's
  // suggestions read it, so they cannot show a different set or a different order. (The server used
  // to derive it too; Go lowercases and compares bytes while the browser uses localeCompare, so the
  // two disagreed on Cyrillic. The server side was removed rather than kept in sync.)
  // Case-insensitive, with a case-sensitive tiebreaker so names differing only by case keep a
  // stable order.
  return Array.from(seen).sort((a, b) =>
    a.localeCompare(b, undefined, { sensitivity: "base" }) || (a < b ? -1 : a > b ? 1 : 0));
}

// SCOPE_GLYPH is our own ≡, drawn so it behaves like a LOWERCASE LETTER of the text beside it: it
// sits just above the baseline, so its bottom edge lines up with "ш" and its top
// edge with "п". A font glyph could not do this — ≡ is drawn on the math axis, which every font
// places differently — and one drawn constant serves both the chip and the menu bullets, so they
// cannot drift apart.
//
// The viewBox is a HALF-PIXEL grid: 16 units render as 8 CSS px, so one unit = 0.5px and the whole
// drawing stays integer while positions finer than a pixel remain expressible. Bars are 2 units
// (1px), gaps 4 units (2px), the band is 14 units (7px), and it sits one unit (0.5px) above the
// element's bottom edge — which is the baseline. That half step is the middle between the two whole
// pixels we tried, reached by scaling the grid instead of writing a fractional offset in CSS.
//
// Everything is a fixed fraction of the element's box, so under zoom the mark scales by exactly the
// zoom factor. The first version was sized in `ex` and drifted against the text between zoom levels:
// `ex` is a font metric the engine resolves (and rounds) per zoom, and the text baseline is snapped
// to whole device pixels by hinting while an SVG box edge is not — so at 100% a 1.2px bar smeared
// across two pixel rows, while at 300% the same absolute error was three times smaller and the true
// geometry showed through. No font metric is involved here any more.
const SCOPE_GLYPH = '<svg class="scopeicon" viewBox="0 0 16 16" aria-hidden="true">'
  + '<rect x="0" y="1" width="16" height="2"></rect>'
  + '<rect x="0" y="7" width="16" height="2"></rect>'
  + '<rect x="0" y="13" width="16" height="2"></rect></svg>';

// renderChip paints the scope chip: indicator and group switcher in ONE element, shaped like a tab so
// it adds no new visual vocabulary. It is absent entirely while no groups exist — until the user
// makes the first group, the bar is exactly what it always was.
// renderChip takes the session list plus the client's per-session unread count, and derives BOTH
// numbers it shows from that one input: the chip's "unread outside this group" and each menu entry's
// own unread. Nothing counts unread twice, in two ways, in two places.
export function renderChip(list, unreadOf){
  const host = document.getElementById("scope");
  if(!host) return;
  const of = typeof unreadOf === "function" ? unreadOf : () => 0;
  const groups = knownGroups(list);
  if(!groups.length && isRoot()){
    host.classList.add("hidden");
    host.innerHTML = "";
    menuOpen = false;
    return;
  }
  host.classList.remove("hidden");
  let outside = 0, total = 0;
  const perGroup = new Map();
  for(const s of list || []){
    const n = of(s.created);
    total += n;
    if(!isRoot() && !inScope(s)) outside += n;
    for(const g of s.groups || []) perGroup.set(g, (perGroup.get(g) || 0) + n);
  }
  // Root is the most crowded scope, so its chip costs the least it can: no label at all, just the
  // glyph, sized like the bar's other glyphs. The name only appears where the strip is shorter.
  // ≡ names the root the same way in both places — as the chip's opener and as the menu's first
  // entry — so "the whole list" has one symbol rather than two competing ones.
  // Every menu row is bulleted with the same ≡: "the whole list" for root, "the list filtered by X"
  // for a group. So root's row is the bare bullet — its name IS the glyph, and printing it twice would
  // only be noise.
  const items = [{ href: "#", label: "", on: isRoot(), n: total }]
    .concat(groups.map(g => ({ href: "#" + encodeScope(g), label: g, on: scope.kind === "group" && scope.name === g, n: perGroup.get(g) || 0 })));
  host.innerHTML =
    '<button type="button" class="scopebtn' + (isRoot() ? " root" : "") + '" title="Группы">'
    + '<span class="scopeglyph">' + SCOPE_GLYPH + "</span>"
    + (isRoot() ? "" : '<span class="scopename">' + esc(scope.name) + "</span>")
    + (outside ? '<span class="scopecount">' + outside + "</span>" : "")
    + "</button>"
    + '<div class="scopemenu hidden">'
    + items.map(i => '<a class="scopeopt' + (i.on ? " sel" : "") + '" href="' + esc(i.href) + '">'
        + '<span class="scopebullet">' + SCOPE_GLYPH + "</span>"
        + (i.label ? '<span class="scopeoptname">' + esc(i.label) + "</span>" : "")
        + (i.n ? '<span class="scopecount">' + i.n + "</span>" : "") + "</a>").join("")
    + "</div>";
  const btn = host.querySelector(".scopebtn"), menu = host.querySelector(".scopemenu");
  if(menuOpen) menu.classList.remove("hidden");
  btn.addEventListener("click", e => {
    e.stopPropagation();
    menuOpen = menu.classList.contains("hidden");
    menu.classList.toggle("hidden", !menuOpen);
  });
  // The entries are real links, so ctrl/middle-click opens a group in a NEW BROWSER TAB natively —
  // which is the entire point of addressing a scope by URL.
  menu.addEventListener("click", () => { menuOpen = false; menu.classList.add("hidden"); });
}

// An open menu is dismissed by a click anywhere else or by Escape — the listener is attached ONCE at
// module load, not per render, because renderChip replaces the chip's DOM on every reconcile.
if(typeof document !== "undefined"){
  document.addEventListener("click", e => {
    if(!menuOpen) return;
    const host = document.getElementById("scope");
    if(host && host.contains(e.target)) return; // the chip's own button/menu handle their clicks
    closeChipMenu();
  });
  document.addEventListener("keydown", e => { if(e.key === "Escape" && menuOpen) closeChipMenu(); });
}

export function closeChipMenu(){
  menuOpen = false;
  const menu = document.querySelector("#scope .scopemenu");
  if(menu) menu.classList.add("hidden");
}
