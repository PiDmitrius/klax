// modal.js — themed confirm/prompt dialog (replaces native confirm/prompt): Escape and
// backdrop click cancel, danger styling for destructive OK, focus management. Resolves to
// false/null on cancel; true (confirm) or the entered string (prompt) on OK.
export function showModal(opts){
  return new Promise(resolve => {
    const m = document.getElementById("modal");
    const inp = m.querySelector(".modal-input");
    const ok = m.querySelector(".modal-ok");
    const cancel = m.querySelector(".modal-cancel");
    m.querySelector(".modal-msg").textContent = opts.message || "";
    if(opts.input){ inp.classList.remove("hidden"); inp.value = opts.value || ""; }
    else inp.classList.add("hidden");
    ok.textContent = opts.okText || "OK";
    ok.classList.toggle("danger", !!opts.danger);
    m.classList.remove("hidden");
    setTimeout(() => { if(opts.input){ inp.focus(); inp.select(); } else ok.focus(); }, 0);
    function done(result){
      m.classList.add("hidden");
      ok.onclick = cancel.onclick = m.onclick = null;
      document.removeEventListener("keydown", onKey, true);
      resolve(result);
    }
    function onKey(e){
      if(e.key === "Escape"){ e.preventDefault(); done(opts.input ? null : false); }
      else if(opts.input && e.key === "Enter" && e.target === inp){ e.preventDefault(); done(inp.value); }
    }
    ok.onclick = () => done(opts.input ? inp.value : true);
    cancel.onclick = () => done(opts.input ? null : false);
    m.onclick = (e) => { if(e.target === m) done(opts.input ? null : false); };
    document.addEventListener("keydown", onKey, true);
  });
}
export function uiConfirm(message, okText, danger){ return showModal({ message, okText, danger }); }
export function uiPrompt(message, value){ return showModal({ message, input: true, value }); }
