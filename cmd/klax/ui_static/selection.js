// selection.js — which tab to focus when the one you were on leaves this window.
//
// Deliberately dependency-free and stateless: it reads nothing about the current scope, the URL or
// the DOM, only two ordered lists. That is what makes the rule testable on its own, and it is the
// ONE rule for every way a tab can leave — closed here, closed in another window, removed from the
// current group, and (later) swept out of a computed view. Four callers cannot drift apart if they
// share this function.

// neighborIn returns the session to focus after `gone` disappears: the one to its LEFT (like a
// browser closing a tab), else the one to its RIGHT. `order` is the order as it was BEFORE the
// change — the departed session must still be in it, or there is no "left" to speak of.
// `survivors`, when given, restricts candidates to sessions that still qualify, so a neighbour that
// left in the same update is skipped rather than selected into a hole.
export function neighborIn(order, gone, survivors){
  const list = order || [];
  const idx = list.findIndex(s => s.created === gone);
  if(idx < 0) return 0;
  const ok = c => !survivors || survivors.some(s => s.created === c);
  for(let i = idx - 1; i >= 0; i--) if(ok(list[i].created)) return list[i].created;
  for(let i = idx + 1; i < list.length; i++) if(ok(list[i].created)) return list[i].created;
  return 0;
}
