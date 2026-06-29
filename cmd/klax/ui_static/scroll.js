// scroll.js — keep the log pinned to the newest message unless the user scrolled up, and
// never rebuild the log out from under a live text selection (that would collapse it). The
// host (app.js) owns the `stick` flag; these are the primitives it drives.

export function nearBottom(el){ return el.scrollHeight - el.scrollTop - el.clientHeight < 80; }
export function toBottom(el){ if(el) el.scrollTop = el.scrollHeight; }

// selectionInLog reports a live, non-collapsed text selection inside el — the signal to
// defer a re-render until the selection clears (flush on selectionchange).
export function selectionInLog(el){
  const sel = typeof document !== "undefined" && document.getSelection && document.getSelection();
  if(!sel || sel.isCollapsed || !sel.rangeCount) return false;
  // True if EITHER endpoint is in the log (a selection extending out of #log still defers).
  try { return (sel.anchorNode && el.contains(sel.anchorNode)) || (sel.focusNode && el.contains(sel.focusNode)) || el.contains(sel.getRangeAt(0).commonAncestorContainer); }
  catch(e){ return false; }
}
