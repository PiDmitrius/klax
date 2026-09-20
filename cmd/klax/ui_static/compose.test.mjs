import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { test } from "node:test";
import { bindButtonActivation } from "./base.js";

let pageCounter=0;
function composer(store = new Map()){
  const pageID="test-page-"+(++pageCounter);
  const elements = {};
  function element(id){
    const listeners = {}, classes = new Set(), parts = {}, attributes = {};
    return elements[id] = {
      id, value: "hello", files: [], disabled: false, inert: false,
      classList: { toggle(k,v){ if(v) classes.add(k); else classes.delete(k); }, contains(k){ return classes.has(k); }, add(k){ classes.add(k); }, remove(k){ classes.delete(k); } },
      addEventListener(k,fn){ (listeners[k] ||= []).push(fn); },
      fire(k,event={}){ for(const fn of listeners[k]||[]) fn({preventDefault(){}, ...event}); },
      querySelectorAll(){ return [elements.input,elements.file,elements.sendbtn,elements.attachbtn]; },
      click(){ this.clicks=(this.clicks||0)+1; },
      setAttribute(k,v){ attributes[k]=v; },
      getAttribute(k){ return attributes[k]; },
      focus(){ ctx.document.activeElement=this; this.fire("focus"); },
      blur(){ this.blurs=(this.blurs||0)+1; if(ctx.document.activeElement===this) ctx.document.activeElement=null; },
      children: [],
      set innerHTML(v){ this.children=[]; },
      appendChild(c){ this.children.push(c); },
      querySelector(k){ return parts[k] ||= element(id+k); },
    };
  }
  for(const id of ["input","file","cbar","sendbtn","attachbtn","chips"]) element(id);
  const apiCalls=[], pending=[], notices=[];
  let cancelCallback;
  const localStorage = {
    getItem: k => store.get(k) ?? null,
    setItem: (k,v) => store.set(k,v),
    removeItem: k => store.delete(k),
    key: i => [...store.keys()][i],
    get length(){ return store.size; },
  };
  let active = 42;
  const deps = {getActive:()=>active, readOnly:()=>false, notice:s=>notices.push(s)};
  const source=readFileSync(new URL("./compose.js",import.meta.url),"utf8").replace(/^import .*;$/gm,"").replace(/export /g,"");
  const ctx={document:{getElementById:id=>elements[id]||null,createElement:()=>element("chip")}, api:(...args)=>{ apiCalls.push(args); return new Promise((resolve,reject)=>{ pending.push({resolve,reject}); args[1].signal.addEventListener("abort",()=>reject(new Error("aborted"))); }); },localStorage,crypto:{randomUUID:()=>pageID},AbortController,FormData,URL,getToken:()=>"",hasCoarsePointer:()=>false,bindButtonActivation,performance:{now:()=>1},setTimeout:(fn,ms)=>{if(ms===3000) cancelCallback=fn; return setTimeout(fn,ms);},clearTimeout};
  runInNewContext(source+"\nthis.inspectFiles=()=>files.length; this.initialize=initCompose; this.access=updateComposerAccess; this.submit=send;",ctx);
  ctx.initialize(deps);
  return {ctx,elements,apiCalls,pending,notices,store,localStorage,deps,
    revealCancel(){ cancelCallback(); },
    switchTo(next){ ctx.saveDraft(active); active=next; ctx.loadDraft(active); ctx.access(false); },
  };
}

test("protected composer disables controls and rejects keyboard, send and attachment events",async()=>{
  const {ctx,elements,apiCalls,deps}=composer();
  deps.readOnly=()=>true;
  ctx.access(true);
  assert.equal(elements.cbar.inert,true);
  assert.equal(elements.cbar.classList.contains("read-only"),true);
  for(const id of ["input","file","sendbtn","attachbtn"]) assert.equal(elements[id].disabled,true);
  elements.input.fire("keydown",{key:"Enter",ctrlKey:true});
  elements.sendbtn.fire("click");
  elements.sendbtn.fire("pointerdown",{pointerType:"touch"});
  elements.attachbtn.fire("click");
  const file={name:"image.png"};
  elements.input.fire("paste",{clipboardData:{items:[{kind:"file",getAsFile:()=>file}]}});
  elements.file.files=[file]; elements.file.fire("change");
  elements.cbar.fire("drop",{dataTransfer:{files:[file]}});
  elements.cbar.fire("dragenter");
  await Promise.resolve();
  assert.equal(ctx.inspectFiles(),0);
  assert.equal(apiCalls.length,0);
  assert.equal(elements.file.clicks,undefined);
  assert.equal(elements.cbar.classList.contains("drag"),false);
  ctx.access(false);
  assert.equal(elements.cbar.inert,false);
  assert.equal(elements.input.disabled,false);
});


test("one pending send keeps the exact draft read-only and blocks every input path",async()=>{
  const h=composer(), {ctx,elements,apiCalls,pending,deps}=h;
  const original="  first\nmessage\n";
  elements.input.value=original;
  const flight=ctx.submit(deps);
  assert.equal(elements.input.value,original);
  assert.equal(elements.input.readOnly,true);
  assert.equal(elements.input.disabled,false);
  assert.equal(elements.sendbtn.disabled,true);
  ctx.access(false);
  assert.equal(elements.input.readOnly,true);
  elements.input.fire("keydown",{key:"Enter"});
  let prevented=false;
  elements.input.fire("beforeinput",{preventDefault(){prevented=true;}});
  assert.equal(prevented,true);
  const file={name:"new.png"};
  elements.input.fire("paste",{clipboardData:{items:[{kind:"file",getAsFile:()=>file}]}});
  elements.file.files=[file]; elements.file.fire("change");
  elements.cbar.fire("drop",{dataTransfer:{files:[file]}});
  elements.attachbtn.fire("click");
  await ctx.submit(deps);
  assert.equal(apiCalls.length,1);
  assert.equal(ctx.inspectFiles(),0);
  assert.equal(elements.file.clicks,undefined);
  pending[0].resolve({status:204}); await flight;
  assert.equal(elements.input.value,"");
  assert.equal(elements.input.readOnly,false);
  assert.equal(elements.sendbtn.disabled,false);
  assert.equal(h.store.size,0);
});

for(const failure of ["http", "network", "body", "unexpected-success", "timeout"]){
  test(`${failure} preserves draft and retry nonce until an explicit 204`,async()=>{
    const h=composer(), {ctx,elements,pending,apiCalls,deps}=h;
    elements.input.value="  exact draft\n";
    let abort;
    if(failure==="timeout") ctx.setTimeout=fn=>{abort=fn; return 0;};
    const flight=ctx.submit(deps);
    if(failure==="network") pending[0].reject(new Error("offline"));
    else if(failure==="timeout") abort();
    else pending[0].resolve({status:failure==="unexpected-success"?200:503,text:async()=>{if(failure==="body") throw new Error("body interrupted"); return "";}});
    await flight;
    assert.equal(elements.input.value,"  exact draft\n");
    assert.equal(elements.input.readOnly,false);
    assert.equal(h.store.size,1);
    assert.equal(h.notices.length,1);
    const second=ctx.submit(deps);
    assert.equal(JSON.parse(apiCalls[1][1].body).nonce,JSON.parse(apiCalls[0][1].body).nonce);
    pending[1].resolve({status:204}); await second;
    assert.equal(elements.input.value,"");
    assert.equal(h.store.size,0);
  });
}

for(const success of [true,false]){
  for(const returnBeforeResponse of [true,false]){
    test(`session switch preserves the owning draft: success=${success}, return=${returnBeforeResponse}`,async()=>{
      const h=composer(), {ctx,elements,pending,deps}=h;
      elements.input.value="other draft"; h.switchTo(7);
      elements.input.value="sending draft";
      const flight=ctx.submit(deps);
      h.switchTo(42);
      assert.equal(elements.input.value,"other draft");
      assert.equal(elements.input.readOnly,false);
      if(returnBeforeResponse) h.switchTo(7);
      pending[0].resolve({status:success?204:503,text:async()=>"rejected"}); await flight;
      if(!returnBeforeResponse){
        assert.equal(elements.input.value,"other draft");
        h.switchTo(7);
      }
      assert.equal(elements.input.value,success?"":"sending draft");
      assert.equal(elements.input.readOnly,false);
      h.switchTo(42); assert.equal(elements.input.value,"other draft");
    });
  }
}

test("reload during send restores the exact submitted text with the original nonce",async()=>{
  const h=composer();
  h.elements.input.value="  durable draft\n";
  const flight=h.ctx.submit(h.deps);
  const reloaded=composer(h.store);
  assert.equal(reloaded.ctx.recoverOutbox({isLive:()=>true}),1);
  reloaded.ctx.loadDraft(42);
  assert.equal(reloaded.elements.input.value,"  durable draft\n");
  const retry=reloaded.ctx.submit(reloaded.deps);
  assert.equal(JSON.parse(reloaded.apiCalls[0][1].body).nonce,JSON.parse(h.apiCalls[0][1].body).nonce);
  reloaded.pending[0].resolve({status:204}); await retry;
  h.pending[0].reject(new Error("lost connection")); await flight;
});

test("failed attachment send retains files and access changes survive completion",async()=>{
  const h=composer();

  h.elements.input.value="";
  h.elements.file.files=[new Blob(["contents"],{type:"text/plain"})];
  h.elements.file.fire("change");
  const flight=h.ctx.submit(h.deps);
  h.elements.chips.children[0].querySelector(".rm").fire("click");
  assert.equal(h.ctx.inspectFiles(),1);
  h.ctx.access(true);
  h.pending[0].reject(new Error("offline")); await flight;
  assert.equal(h.ctx.inspectFiles(),1);
  assert.equal(h.elements.input.readOnly,true);
  assert.equal(h.elements.input.disabled,true);
  h.ctx.access(false);
  const retry=h.ctx.submit(h.deps);
  assert.equal(h.apiCalls[1][1].body.get("nonce"),h.apiCalls[0][1].body.get("nonce"));
  h.pending[1].resolve({status:204}); await retry;
  assert.equal(h.ctx.inspectFiles(),0);
});

test("storage refusal leaves the draft editable and sends no request",async()=>{
  const h=composer();
  h.localStorage.setItem=()=>{throw new Error("quota");};
  await h.ctx.submit(h.deps);
  assert.equal(h.elements.input.value,"hello");
  assert.notEqual(h.elements.input.readOnly,true);
  assert.equal(h.apiCalls.length,0);
  assert.equal(h.notices.length,1);
});


test("multiple recovered drafts surface separately after each confirmation",async()=>{
  const h=composer();
  for(const [nonce,text] of [["old-a","first"],["old-b","second"]]){
    h.ctx.outboxPut({created:42,nonce,text,sent:true});
  }
  h.ctx.recoverOutbox({isLive:()=>true}); h.ctx.loadDraft(42);
  for(const [index,text,nonce] of [[0,"first","old-a"],[1,"second","old-b"]]){
    assert.equal(h.elements.input.value,text);
    const flight=h.ctx.submit(h.deps);
    assert.equal(JSON.parse(h.apiCalls[index][1].body).nonce,nonce);
    h.pending[index].resolve({status:204}); await flight;
  }
  assert.equal(h.elements.input.value,"");
  assert.equal(h.store.size,0);
});

test("an uncommitted overwrite cannot count as a durable save",()=>{
  const h=composer();
  h.ctx.outboxPut({created:42,nonce:"same",text:"old"});
  h.localStorage.setItem=()=>{};
  assert.equal(h.ctx.outboxPut({created:42,nonce:"same",text:"new"}),false);
});


for(const outcome of ["reject", "timeout", "accept"]){
  test(`mobile send clears focus once, blocks refocus while pending, and allows a fresh tap after ${outcome}`,async()=>{
    const h=composer(), {ctx,elements,deps,pending}=h;
    ctx.hasCoarsePointer=()=>true;
      let abort;
    if(outcome==="timeout") ctx.setTimeout=fn=>{abort=fn; return 0;};
    elements.input.focus();
    const flight=ctx.submit(deps);
    assert.equal(ctx.document.activeElement,null);
    assert.equal(elements.input.blurs,1);
    assert.equal(elements.cbar.getAttribute("aria-busy"),"true");
    assert.equal(elements.sendbtn.getAttribute("aria-label"),"Отправка…");
    ctx.access(false);
    assert.equal(elements.input.blurs,1);
    let prevented=false;
    elements.input.fire("pointerdown",{preventDefault(){prevented=true;}});
    assert.equal(prevented,true);
    elements.input.focus();
    assert.equal(ctx.document.activeElement,null);
    if(outcome==="timeout") abort();
    else pending[0].resolve({status:outcome==="accept"?204:503,text:async()=>"rejected"});
    await flight;
    assert.equal(ctx.document.activeElement,null);
    assert.equal(elements.input.value,outcome==="accept"?"":"hello");
    assert.equal(elements.input.readOnly,false);
    assert.equal(elements.cbar.getAttribute("aria-busy"),"false");
    assert.equal(elements.sendbtn.getAttribute("aria-label"),"Отправить");
    prevented=false;
    elements.input.fire("pointerdown",{preventDefault(){prevented=true;}});
    assert.equal(prevented,false);
    elements.input.focus();
    ctx.access(false);
    assert.equal(ctx.document.activeElement,elements.input);
  });
}

test("desktop send retains input focus and background completion does not steal it",async()=>{
  const h=composer();

  h.elements.input.focus();
  const flight=h.ctx.submit(h.deps);
  assert.equal(h.ctx.document.activeElement,h.elements.input);
  h.switchTo(7);
  h.elements.attachbtn.focus();
  h.pending[0].resolve({status:204}); await flight;
  assert.equal(h.ctx.document.activeElement,h.elements.attachbtn);
});

test("refreshing access does not rewrite the textarea editing state",()=>{
  const h=composer();
  let state=false, writes=0;
  Object.defineProperty(h.elements.input,"readOnly",{get:()=>state,set:v=>{state=v; writes++;}});
  h.ctx.access(false);
  h.ctx.access(false);
  assert.equal(writes,0);
  h.ctx.access(true);
  h.ctx.access(true);
  assert.equal(writes,1);
  h.ctx.access(false);
  assert.equal(writes,2);
});


test("mobile button locks before the request and consumes the click after a fast rejection",async()=>{
  const h=composer();
  h.ctx.hasCoarsePointer=()=>true;

  h.elements.input.focus();
  let prevented=false;
  h.elements.sendbtn.fire("pointerdown",{pointerType:"touch",preventDefault(){prevented=true;}});
  assert.equal(prevented,true);
  assert.equal(h.apiCalls.length,1);
  assert.equal(h.ctx.document.activeElement,null);
  assert.equal(h.elements.input.readOnly,true);
  h.pending[0].resolve({status:503,text:async()=>"rejected"});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(h.elements.input.readOnly,false);
  h.elements.sendbtn.fire("click");
  assert.equal(h.apiCalls.length,1);
  h.elements.input.focus();
  h.elements.sendbtn.fire("pointerdown",{pointerType:"touch"});
  assert.equal(h.apiCalls.length,2);
  assert.equal(h.ctx.document.activeElement,null);
  h.pending[1].resolve({status:204});
  await new Promise(resolve=>setImmediate(resolve));
  h.elements.sendbtn.fire("click");
  assert.equal(h.apiCalls.length,2);
  assert.equal(h.elements.input.value,"");
});


for(const coarse of [true,false]){
  test(`cancel button releases a pending send and preserves its draft: coarse=${coarse}`,async()=>{
    const h=composer();
    h.ctx.hasCoarsePointer=()=>coarse;

    const flight=h.ctx.submit(h.deps);
    h.revealCancel();
    assert.equal(h.elements.sendbtn.disabled,false);
    assert.equal(h.elements.cbar.inert,false);
    assert.equal(h.elements.cbar.classList.contains("read-only"),true);
    if(coarse){
      h.elements.sendbtn.fire("pointerdown",{pointerType:"touch"});
      h.elements.sendbtn.fire("click");
    } else h.elements.sendbtn.fire("click");
    await flight;
    assert.equal(h.apiCalls[0][1].signal.aborted,true);
    assert.equal(h.apiCalls.length,1);
    assert.equal(h.elements.input.value,"hello");
    assert.equal(h.elements.input.readOnly,false);
    assert.equal(h.elements.cbar.classList.contains("read-only"),false);
    assert.equal(h.store.size,1);
    assert.match(h.notices[0],/Ожидание отменено/);
    const retry=h.ctx.submit(h.deps);
    assert.equal(JSON.parse(h.apiCalls[1][1].body).nonce,JSON.parse(h.apiCalls[0][1].body).nonce);
    h.pending[1].resolve({status:204}); await retry;
    assert.equal(h.elements.input.value,"");
  });
}

test("editing after cancellation uses a new durable nonce",async()=>{
  const h=composer();
  const first=h.ctx.submit(h.deps);
  h.revealCancel();
  h.elements.sendbtn.fire("click"); await first;
  h.elements.input.value="edited"; h.elements.input.fire("input");
  const second=h.ctx.submit(h.deps);
  assert.notEqual(JSON.parse(h.apiCalls[1][1].body).nonce,JSON.parse(h.apiCalls[0][1].body).nonce);
  assert.equal(JSON.parse(h.apiCalls[1][1].body).text,"edited");
  h.pending[0].resolve({status:204});
  assert.equal(h.elements.input.value,"edited");
  h.pending[1].resolve({status:204}); await second;
});

test("cancellation stays with the sending session and preserves both drafts",async()=>{
  const h=composer();
  h.elements.input.value="other"; h.switchTo(7);
  h.elements.input.value="pending";
  const flight=h.ctx.submit(h.deps);
  h.switchTo(42);
  h.revealCancel();
  assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
  assert.equal(h.elements.input.readOnly,false);
  h.switchTo(7);
  assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),true);
  h.elements.sendbtn.fire("click"); await flight;
  h.switchTo(42);
  assert.equal(h.elements.input.value,"other");
  assert.equal(h.elements.input.readOnly,false);
  h.switchTo(7); assert.equal(h.elements.input.value,"pending");
});


for(const status of [204,503]){
  test(`fast response ${status} never reveals cancellation`,async()=>{
    const h=composer();
    const timers=new Map(); let id=0;
    h.ctx.setTimeout=(fn,ms)=>{timers.set(++id,{fn,ms}); return id;};
    h.ctx.clearTimeout=id=>timers.delete(id);
    const flight=h.ctx.submit(h.deps);
    assert.equal(h.elements.sendbtn.disabled,true);
    assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
    h.elements.sendbtn.fire("click");
    assert.equal(h.apiCalls[0][1].signal.aborted,false);
    const delayed=[...timers.values()].find(t=>t.ms===3000).fn;
    h.pending[0].resolve({status,text:async()=>"rejected"}); await flight;
    assert.equal(timers.size,0);
    delayed();
    assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
    assert.equal(h.elements.sendbtn.disabled,false);
  });
}

test("cancel appears only after its delay and resets for the next request",async()=>{
  const h=composer();
  const first=h.ctx.submit(h.deps);
  assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
  h.revealCancel();
  assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),true);
  assert.equal(h.elements.sendbtn.getAttribute("aria-label"),"Отменить ожидание отправки");
  h.ctx.access(false);
  assert.equal(h.elements.sendbtn.disabled,false);
  h.elements.sendbtn.fire("click"); await first;
  const next=h.ctx.submit(h.deps);
  assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
  assert.equal(h.elements.sendbtn.disabled,true);
  h.pending[1].resolve({status:204}); await next;
});

function typeDraft(h,text){
  h.elements.input.value=text;
  h.elements.input.fire("input");
}
function entries(h){ return [...h.store.values()].map(v=>JSON.parse(v)); }
function restored(store){
  const h=composer(store);
  h.ctx.recoverOutbox({isLive:()=>true}); h.ctx.loadDraft(42);
  return h;
}

test("typing, reloading, and sending use one outbox entry with exact text",async()=>{
  const h=composer();
  typeDraft(h,"first");
  typeDraft(h,"  final\ntext\n");
  assert.equal(h.store.size,1);
  const saved=entries(h)[0];
  assert.equal(saved.text,"  final\ntext\n");
  assert.equal(saved.sent,false);
  const next=restored(h.store);
  assert.equal(next.elements.input.value,saved.text);
  const flight=next.ctx.submit(next.deps);
  assert.equal(next.store.size,1);
  assert.equal(entries(next)[0].sent,true);
  assert.equal(entries(next)[0].nonce,saved.nonce);
  next.pending[0].resolve({status:204}); await flight;
  assert.equal(next.store.size,0);
  assert.equal(restored(next.store).elements.input.value,"");
});

test("drafts survive reload separately for each session",()=>{
  const h=composer();
  typeDraft(h,"session 42"); h.switchTo(7); typeDraft(h,"session 7");
  assert.equal(h.store.size,2);
  const next=restored(h.store);
  assert.equal(next.elements.input.value,"session 42");
  next.switchTo(7); assert.equal(next.elements.input.value,"session 7");
  next.switchTo(42); assert.equal(next.elements.input.value,"session 42");
});

test("independent browser tabs cannot overwrite drafts for the same session",()=>{
  const first=composer(), second=composer(first.store);

  typeDraft(first,"first tab"); typeDraft(second,"second tab"); typeDraft(first,"first tab edit");
  assert.deepEqual(entries(first).map(e=>e.text).sort(),["first tab edit","second tab"]);
});

test("two tabs editing the same recovered draft preserve both versions",()=>{
  const original=composer(); typeDraft(original,"original");
  const first=restored(original.store), second=restored(original.store);
  typeDraft(first,"first edit"); typeDraft(second,"second edit");
  assert.deepEqual(entries(first).map(e=>e.text).sort(),["first edit","second edit"]);
  assert.notEqual(entries(first)[0].nonce,entries(first)[1].nonce);
});

test("acceptance in one tab cannot erase an edit in another",async()=>{
  const original=composer(); typeDraft(original,"original");
  const first=restored(original.store), second=restored(original.store);
  const flight=first.ctx.submit(first.deps);
  typeDraft(second,"new version");
  first.pending[0].resolve({status:204}); await flight;
  assert.equal(first.store.size,1);
  assert.equal(restored(first.store).elements.input.value,"new version");
});

test("clearing a recovered draft discards it and reveals the next saved draft",()=>{
  const first=composer(), second=composer(first.store);

  typeDraft(first,"first"); typeDraft(second,"second");
  const h=restored(first.store);
  assert.equal(h.elements.input.value,"first");
  typeDraft(h,"");
  assert.equal(h.elements.input.value,"second");
  assert.equal(h.store.size,1);
  typeDraft(h,"");
  assert.equal(h.store.size,0);
  assert.equal(restored(h.store).elements.input.value,"");
});

test("whitespace-only drafts survive reload unchanged",()=>{
  const h=composer(); typeDraft(h," \n  ");
  assert.equal(restored(h.store).elements.input.value," \n  ");
});

test("storage failure retains prior text, warns once, and prevents transmission",async()=>{
  const h=composer(); typeDraft(h,"saved");
  const write=h.localStorage.setItem;
  h.localStorage.setItem=()=>{throw new Error("full");};
  typeDraft(h,"new"); typeDraft(h,"newer");
  assert.equal(h.notices.length,1);
  assert.equal(entries(h)[0].text,"saved");
  assert.equal(h.elements.input.value,"newer");
  await h.ctx.submit(h.deps);
  assert.equal(h.apiCalls.length,0);
  h.localStorage.setItem=write;
  typeDraft(h,"newest");
  assert.equal(h.store.size,1);
  assert.equal(restored(h.store).elements.input.value,"newest");
});

test("typed draft retains its stored nonce through cancellation and retry",async()=>{
  const h=composer(); typeDraft(h,"saved before send");
  const nonce=entries(h)[0].nonce;
  const first=h.ctx.submit(h.deps); h.revealCancel(); h.elements.sendbtn.fire("click"); await first;
  assert.equal(entries(h)[0].nonce,nonce);
  assert.equal(h.elements.input.value,"saved before send");
  const next=restored(h.store);
  const retry=next.ctx.submit(next.deps);
  assert.equal(JSON.parse(next.apiCalls[0][1].body).nonce,nonce);
  next.pending[0].resolve({status:204}); await retry;
  assert.equal(next.store.size,0);
});

test("read-only input events cannot create stored drafts",()=>{
  const h=composer(); h.deps.readOnly=()=>true;
  typeDraft(h,"not writable");
  assert.equal(h.store.size,0);
});


for(const text of ["", "unchanged text"]){
  for(const edit of ["add", "remove"]){
    test(`attachment ${edit} after cancellation rotates nonce: text=${!!text}`,async()=>{
      const h=composer(); typeDraft(h,text);
      h.elements.file.files=[new Blob(["first"],{type:"text/plain"})]; h.elements.file.fire("change");
      const first=h.ctx.submit(h.deps); h.revealCancel(); h.elements.sendbtn.fire("click"); await first;
      if(edit==="add"){
        h.elements.file.files=[new Blob(["second"],{type:"text/plain"})]; h.elements.file.fire("change");
      } else {
        h.elements.chips.children[0].querySelector(".rm").fire("click");
        if(!text){ h.elements.file.files=[new Blob(["replacement"],{type:"text/plain"})]; h.elements.file.fire("change"); }
      }
      h.switchTo(7); h.switchTo(42);
      const second=h.ctx.submit(h.deps);
      const a=h.apiCalls[0][1].body.get("nonce");
      const body=h.apiCalls[1][1].body;
      const b=typeof body==="string"?JSON.parse(body).nonce:body.get("nonce");
      h.pending[1].resolve({status:204}); await second;
      assert.notEqual(a,b);
    });
  }
}


test("closing a session discards only its never-transmitted drafts",()=>{
  const h=composer(); typeDraft(h,"discard on close");
  h.switchTo(7); typeDraft(h,"keep live session");
  h.ctx.outboxPut({created:42,nonce:"pending",text:"uncertain",sent:true});
  h.ctx.outboxPut({created:42,nonce:"legacy",text:"unknown submission"});
  h.ctx.dropDraft(42, true);
  assert.deepEqual(entries(h).map(e=>e.text).sort(),["keep live session","uncertain","unknown submission"]);
});

test("recovery reclaims closed unsent drafts and preserves uncertain or legacy entries",()=>{
  const h=composer();
  h.ctx.outboxPut({created:7,nonce:"typed",text:"unsent",sent:false});
  h.ctx.outboxPut({created:7,nonce:"sent",text:"uncertain",sent:true});
  h.ctx.outboxPut({created:7,nonce:"legacy",text:"legacy"});
  h.ctx.outboxPut({created:42,nonce:"live",text:"keep",sent:false});
  const notices=[];
  assert.equal(h.ctx.recoverOutbox({isLive:c=>c===42,notice:s=>notices.push(s)},h.ctx.outboxList()),1);
  assert.deepEqual(entries(h).map(e=>e.nonce).sort(),["legacy","live","sent"]);
  assert.match(notices[1],/: 2$/);
});

test("closed unsent drafts cannot permanently exhaust outbox capacity",async()=>{
  const h=composer();
  for(let i=0;i<500;i++) assert.equal(h.ctx.outboxPut({created:7,nonce:"closed-"+i,text:"draft",sent:false}),true);
  assert.equal(h.ctx.recoverOutbox({isLive:c=>c===42,notice:s=>h.notices.push(s)},h.ctx.outboxList()),0);
  assert.equal(h.store.size,0);
  assert.equal(h.notices.length,0);
  typeDraft(h,"live draft");
  const flight=h.ctx.submit(h.deps);
  assert.equal(h.apiCalls.length,1);
  h.pending[0].resolve({status:204}); await flight;
});


test("a delayed session snapshot cannot erase another tab's newly created draft",()=>{
  const first=composer(), second=composer(first.store);

  const beforeRequest=first.ctx.outboxList();
  second.switchTo(7); typeDraft(second,"new session draft");
  first.ctx.recoverOutbox({isLive:c=>c===42},beforeRequest);
  assert.equal(first.store.size,1);
  const next=restored(first.store); next.switchTo(7);
  assert.equal(next.elements.input.value,"new session draft");
});

test("unconfirmed session disappearance retains its durable draft",()=>{
  const h=composer(); typeDraft(h,"keep until confirmed");
  h.ctx.dropDraft(42);
  assert.equal(restored(h.store).elements.input.value,"keep until confirmed");
});

for(const outcome of ["accept", "reject", "cancel", "timeout"]){
  test(`concurrent session sends remain independent when the background send ends: ${outcome}`,async()=>{
    const h=composer();
    const timers=[];
    h.ctx.setTimeout=(fn,ms)=>{timers.push({fn,ms}); return timers.length;};
    h.ctx.clearTimeout=()=>{};
    typeDraft(h,"first draft");
    const first=h.ctx.submit(h.deps);
    const firstNonce=JSON.parse(h.apiCalls[0][1].body).nonce;
    h.switchTo(7);
    assert.equal(h.elements.input.readOnly,false);
    assert.equal(h.elements.cbar.getAttribute("aria-busy"),"false");
    typeDraft(h,"second draft");
    h.elements.file.files=[new Blob(["attachment"],{type:"text/plain"})];
    h.elements.file.fire("change");
    assert.equal(h.ctx.inspectFiles(),1);
    const second=h.ctx.submit(h.deps);
    assert.equal(h.apiCalls.length,2);
    await h.ctx.submit(h.deps);
    assert.equal(h.apiCalls.length,2);
    assert.equal(h.elements.input.readOnly,true);
    timers[0].fn();
    assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),false);
    if(outcome==="cancel"){
      h.switchTo(42);
      assert.equal(h.elements.sendbtn.classList.contains("cancel-send"),true);
      h.elements.sendbtn.fire("click");
      h.switchTo(7);
    } else if(outcome==="timeout") timers[1].fn();
    else h.pending[0].resolve({status:outcome==="accept"?204:503,text:async()=>"rejected"});
    await first;
    assert.equal(h.elements.input.value,"second draft");
    assert.equal(h.elements.input.readOnly,true);
    assert.equal(h.elements.cbar.getAttribute("aria-busy"),"true");
    assert.equal(h.ctx.inspectFiles(),1);
    assert.equal(h.apiCalls[1][1].signal.aborted,false);
    assert.equal(h.ctx.outboxList().some(e=>e.nonce===firstNonce),outcome!=="accept");
    h.switchTo(42);
    assert.equal(h.elements.input.value,outcome==="accept"?"":"first draft");
    assert.equal(h.elements.input.readOnly,false);
    h.pending[1].resolve({status:204}); await second;
    assert.equal(h.elements.input.value,outcome==="accept"?"":"first draft");
    h.switchTo(7);
    assert.equal(h.elements.input.value,"");
    assert.equal(h.ctx.inspectFiles(),0);
    assert.equal(h.elements.input.readOnly,false);
  });
}

test("background cancellation timer and completion preserve mobile editing in another session",async()=>{
  const h=composer(); h.ctx.hasCoarsePointer=()=>true;
  typeDraft(h,"pending");
  const first=h.ctx.submit(h.deps);
  h.switchTo(7);
  h.elements.input.focus();
  typeDraft(h,"new draft");
  h.revealCancel();
  assert.equal(h.ctx.document.activeElement,h.elements.input);
  assert.equal(h.elements.input.readOnly,false);
  assert.equal(h.elements.sendbtn.title,"Отправить");
  assert.equal(h.ctx.outboxList().find(e=>e.created===7).text,"new draft");
  h.pending[0].resolve({status:204}); await first;
  assert.equal(h.ctx.document.activeElement,h.elements.input);
  assert.equal(h.elements.input.value,"new draft");
});
