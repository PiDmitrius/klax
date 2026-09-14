import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";

const tick = () => new Promise(resolve => setImmediate(resolve));
function harness(saved="",hash=""){
  const store=new Map(saved ? [["klax_ui_token",saved]] : []), elements={}, calls=[];
  for(const id of ["token","tokenbtn","autherror","gate","app"]){
    const classes=new Set(), listeners={};
    elements[id]={value:"",textContent:"",disabled:false,
      classList:{add:c=>classes.add(c),remove:c=>classes.delete(c),contains:c=>classes.has(c)},
      addEventListener:(key,fn)=>(listeners[key]||=[]).push(fn),
      fire:(key,e={})=>{for(const fn of listeners[key]||[])fn(e);},
      listeners};
  }
  let starts=0,reloads=0;
  const ctx={URLSearchParams,
    localStorage:{getItem:k=>store.get(k)||null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)},
    location:{pathname:"/mount/",search:"",hash,reload:()=>reloads++},
    history:{replaceState:(_s,_t,url)=>{ctx.location.hash="";ctx.cleanedURL=url;}},
    document:{getElementById:id=>elements[id]},
    fetch:(url,options)=>new Promise(resolve=>calls.push({url,options,resolve})),
  };
  const source=["base.js","auth.js"].map(name=>readFileSync(new URL(name,import.meta.url),"utf8").replace(/^import .*;\n/gm,"").replace(/export /g,"")).join("\n");
  runInNewContext(source+"\nthis.boot=initAuth; this.readOnly=isReadOnly; this.api=api; this.setToken=setToken; this.getToken=getToken;",ctx);
  const respond=(n,status=200,read_only=false)=>calls[n].resolve({status,ok:status===200,json:async()=>({user:"owner",read_only})});
  ctx.boot(()=>{starts++;elements.gate.classList.add("hidden");elements.app.classList.add("active");});
  return {ctx,store,elements,calls,respond,starts:()=>starts,reloads:()=>reloads};
}

test("an invalid saved token returns to a working login form",async()=>{
  const h=harness("expired");
  h.respond(0,401);await tick();
  assert.equal(h.store.has("klax_ui_token"),false);
  assert.equal(h.starts(),0);
  assert.equal(h.elements.gate.classList.contains("hidden"),false);
  h.elements.token.value="reader";
  h.elements.token.fire("keydown",{key:"Enter"});
  h.elements.tokenbtn.fire("click");
  assert.equal(h.calls.length,2);
  h.respond(1,200,true);await tick();
  assert.equal(h.starts(),1);
  assert.equal(h.ctx.readOnly(),true);
  assert.equal(h.store.get("klax_ui_token"),"reader");
  assert.equal(h.elements.token.value,"");
});

test("login fragment replaces cached management token and is absent from requests",async()=>{
  const h=harness("manager","#login=view%2Bsecret");
  assert.equal(h.ctx.cleanedURL,"/mount/");
  assert.equal(h.ctx.location.hash,"");
  assert.equal(h.calls[0].url,"/mount/api/auth");
  assert.equal(h.calls[0].options.headers.Authorization,"Bearer view+secret");
  h.respond(0,200,true);await tick();
  assert.equal(h.starts(),1);
  assert.equal(h.ctx.readOnly(),true);
});

test("authorization loss clears the credential and reloads the initialized app once",async()=>{
  const h=harness("manager");h.respond(0);await tick();
  const first=h.ctx.api("/api/sessions"),second=h.ctx.api("/api/tail");
  h.respond(1,401);h.respond(2,401);await Promise.all([first,second]);
  assert.equal(h.reloads(),1);
  assert.equal(h.ctx.getToken(),"");
  assert.equal(h.store.has("klax_ui_token"),false);
});

test("a delayed unauthorized response cannot invalidate a replacement credential",async()=>{
  const h=harness();
  h.ctx.setToken("old");
  const pending=h.ctx.api("/api/sessions");
  h.ctx.setToken("new");h.respond(0,401);await pending;
  assert.equal(h.ctx.getToken(),"new");
  assert.equal(h.reloads(),0);
});

test("read access rejects draft creation and tab dragging before touching the UI",()=>{
  const source=readFileSync(new URL("tabs.js",import.meta.url),"utf8").replace(/^import .*;\n/gm,"").replace(/export /g,"");
  const ctx={isReadOnly:()=>true,document:{title:"test",getElementById(){throw Error("unexpected UI mutation");},querySelector(){throw Error("unexpected UI mutation");}}};
  runInNewContext(source+"\nthis.open=openDraft; this.drag=startDrag;",ctx);
  ctx.open();ctx.drag({},{});
});
