// modal.js — themed confirm/prompt dialog (replaces native confirm/prompt): Escape and
// backdrop click cancel, danger styling for destructive OK, focus management. Resolves to
// false/null on cancel; true (confirm) or the entered string (prompt) on OK.
import { hasCoarsePointer } from "./base.js";

export function showModal(opts){
  return new Promise(resolve => {
    const m = document.getElementById("modal");
    const inp = m.querySelector(".modal-input");
    const ok = m.querySelector(".modal-ok");
    const cancel = m.querySelector(".modal-cancel");
    const box = m.querySelector(".modal-box");
    const previousFocus = document.activeElement;
    m.querySelector(".modal-msg").textContent = opts.message || "";
    if(opts.input){ inp.classList.remove("hidden"); inp.value = opts.value || ""; }
    else inp.classList.add("hidden");
    ok.textContent = opts.okText || "OK";
    ok.classList.toggle("danger", !!opts.danger);
    m.classList.remove("hidden");
    // A confirmation owns focus, but no action is implicitly selected: :focus-visible is deliberately
    // browser/input-modality dependent, so focusing either button made its outline appear at random.
    // The neutral box is the stable initial target; the first Tab moves to Cancel.
    setTimeout(() => { if(opts.input){ inp.focus(); inp.select(); } else box.focus({ preventScroll: true }); }, 0);
    function done(result){
      m.classList.add("hidden");
      ok.onclick = cancel.onclick = m.onclick = null;
      document.removeEventListener("keydown", onKey, true);
      // Run after the closing click's default action; otherwise some browsers focus the clicked
      // (now hidden) button again after this handler and undo the restoration.
      setTimeout(() => {
        if(typeof opts.returnFocus === "function"){ opts.returnFocus(); return; }
        const target = opts.returnFocus || previousFocus;
        const mobileText = hasCoarsePointer() && target && target.matches && target.matches("input, textarea, [contenteditable=true]");
        if(!mobileText && target && target.isConnected && target.focus){
          try { target.focus({ preventScroll: true }); } catch(e){ target.focus(); }
        }
      }, 0);
      resolve(result);
    }
    function onKey(e){
      if(e.key === "Escape"){ e.preventDefault(); done(opts.input ? null : false); }
      else if(opts.input && e.key === "Enter" && e.target === inp){ e.preventDefault(); done(inp.value); }
      else if(e.key === "Tab"){
        const items = opts.input ? [inp, cancel, ok] : [cancel, ok];
        const i = items.indexOf(document.activeElement);
        const next = e.shiftKey ? (i <= 0 ? items.length - 1 : i - 1) : (i < 0 || i === items.length - 1 ? 0 : i + 1);
        e.preventDefault(); items[next].focus();
      }
    }
    ok.onclick = () => done(opts.input ? inp.value : true);
    cancel.onclick = () => done(opts.input ? null : false);
    m.onclick = (e) => { if(e.target === m) done(opts.input ? null : false); };
    document.addEventListener("keydown", onKey, true);
  });
}
export function uiConfirm(message, okText, danger, returnFocus){ return showModal({ message, okText, danger, returnFocus }); }
export function uiPrompt(message, value){ return showModal({ message, input: true, value }); }
