// scroll.js — keep the log pinned to the newest message unless the user scrolled up, and keep a
// text selection across re-renders. The host (app.js) owns the `stick` flag; these are the
// primitives it drives.

// selectionInLog reports a live, non-collapsed text selection with an endpoint inside el.
export function selectionInLog(el){
  const sel = typeof document !== "undefined" && document.getSelection && document.getSelection();
  if(!sel || sel.isCollapsed || !sel.rangeCount) return false;
  try { return (sel.anchorNode && el.contains(sel.anchorNode)) || (sel.focusNode && el.contains(sel.focusNode)) || el.contains(sel.getRangeAt(0).commonAncestorContainer); }
  catch(e){ return false; }
}

// A selection endpoint inside the log is kept as its top-level item (render key) and a character
// offset in the item's message text (the .body elements, in order). That text does not change when
// the unread line splits or merges a message, or when a message node is patched, rebuilt or moved.
// An endpoint outside the log is kept as is.
function inBody(el){ return el.nodeType === 1 && el.classList.contains("body"); }
function textOffset(root, node, offset){
  let n = 0, found = false;
  const visit = (el, body) => {
    body = body || inBody(el);
    if(el === node){
      if(el.nodeType === 3){ if(body) n += offset; }
      else for(let i = 0; i < offset; i++) visit(el.childNodes[i], body);
      found = true;
      return;
    }
    if(el.nodeType === 3){ if(body) n += el.data.length; return; }
    for(const c of el.childNodes){ visit(c, body); if(found) return; }
  };
  visit(root, false);
  return found ? n : null;
}
function textPoint(root, chars){
  let last = null;
  const visit = (el, body) => {
    body = body || inBody(el);
    if(el.nodeType === 3){
      if(!body) return null;
      if(chars <= el.data.length) return [el, chars];
      chars -= el.data.length; last = el;
      return null;
    }
    for(const c of el.childNodes){ const p = visit(c, body); if(p) return p; }
    return null;
  };
  return visit(root, false) || (last ? [last, last.data.length] : null);
}
function savePoint(col, node, offset){
  if(!col.contains(node)) return { node, offset };
  const el = node.nodeType === 3 ? node.parentElement : node;
  const body = el && el.closest(".body"), top = body && body.closest("[data-render-key]");
  if(!top || !col.contains(top)) return null;
  const chars = textOffset(top, node, offset);
  return chars === null ? null : { key: top.dataset.renderKey, chars };
}
function findPoint(col, p){
  if(!p.key) return p.node.isConnected ? [p.node, p.offset] : null;
  const top = Array.from(col.children).find(c => c.dataset && c.dataset.renderKey === p.key);
  return top ? textPoint(top, p.chars) : null;
}

// saveSelection records the log's selection before a render; restoreSelection puts it back after.
export function saveSelection(col){
  if(!col || !selectionInLog(col)) return null;
  const sel = document.getSelection();
  const anchor = savePoint(col, sel.anchorNode, sel.anchorOffset), focus = savePoint(col, sel.focusNode, sel.focusOffset);
  return anchor && focus ? { anchor, focus, raw: [sel.anchorNode, sel.anchorOffset, sel.focusNode, sel.focusOffset] } : null;
}
export function restoreSelection(col, saved){
  if(!saved) return;
  const sel = document.getSelection(), [an, ao, fn, fo] = saved.raw;
  if(an.isConnected && fn.isConnected && sel.anchorNode === an && sel.anchorOffset === ao && sel.focusNode === fn && sel.focusOffset === fo) return;
  const a = findPoint(col, saved.anchor), f = findPoint(col, saved.focus);
  if(a && f) sel.setBaseAndExtent(a[0], a[1], f[0], f[1]);
}
