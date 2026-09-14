import { api, getToken, setToken, onAuthFailure } from "./base.js";

let readOnly = true;
export function isReadOnly(){ return readOnly; }

export function consumeLoginLink(){
  if(!location.hash.startsWith("#login=")) return;
  const params = new URLSearchParams(location.hash.slice(1));
  const token = params.get("login") || "";
  history.replaceState(null, "", location.pathname + location.search);
  setToken(token);
}

export function initAuth(start){
  let checking = false, started = false, invalidated = false;
  const input = document.getElementById("token");
  const button = document.getElementById("tokenbtn");
  const message = document.getElementById("autherror");
  const fail = text => {
    document.getElementById("gate").classList.remove("hidden");
    document.getElementById("app").classList.remove("active");
    if(message) message.textContent = text;
  };
  onAuthFailure(() => {
    if(invalidated) return;
    setToken("");
    if(started){
      invalidated = true;
      location.reload();
    } else {
      fail("Токен недействителен. Введите действующий токен.");
    }
  });
  const login = async token => {
    if(checking || started || !token) return;
    checking = true;
    button.disabled = true;
    setToken(token);
    try {
      const r = await api("/api/auth");
      if(!r.ok){
        if(r.status !== 401) fail("Не удалось войти. Попробуйте ещё раз.");
        return;
      }
      const access = await r.json();
      readOnly = !!access.read_only;
      input.value = "";
      if(message) message.textContent = "";
      started = true;
      start();
    } catch(_){ fail("Не удалось связаться с сервером. Попробуйте ещё раз."); }
    finally { checking = false; button.disabled = false; }
  };
  button.addEventListener("click", () => login(input.value.trim()));
  input.addEventListener("keydown", e => { if(e.key === "Enter") login(input.value.trim()); });
  consumeLoginLink();
  if(getToken()) login(getToken());
}
