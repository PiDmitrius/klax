import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { test } from "node:test";
import { bindButtonActivation } from "./base.js";

function composer(){
  const elements = {};
  function element(id){
    const listeners = {}, classes = new Set();
    return elements[id] = {
      value: "hello", files: [], disabled: false, inert: false,
      classList: { toggle(k,v){ if(v) classes.add(k); else classes.delete(k); }, contains(k){ return classes.has(k); }, add(k){ classes.add(k); }, remove(k){ classes.delete(k); } },
      addEventListener(k,fn){ (listeners[k] ||= []).push(fn); },
      fire(k,event={}){ for(const fn of listeners[k]||[]) fn({preventDefault(){}, ...event}); },
      querySelectorAll(){ return [elements.input,elements.file,elements.sendbtn,elements.attachbtn]; },
      click(){ this.clicks=(this.clicks||0)+1; },
    };
  }
  for(const id of ["input","file","cbar","sendbtn","attachbtn"]) element(id);
  const apiCalls=[];
  const source=readFileSync(new URL("./compose.js",import.meta.url),"utf8").replace(/^import .*;$/gm,"").replace(/export /g,"");
  const ctx={document:{getElementById:id=>elements[id]||null}, api:(...args)=>apiCalls.push(args),getToken:()=>"",hasCoarsePointer:()=>false,bindButtonActivation,performance:{now:()=>1},setTimeout,clearTimeout};
  runInNewContext(source+"\nthis.inspectFiles=()=>files.length; this.initialize=initCompose; this.access=updateComposerAccess;",ctx);
  return {ctx,elements,apiCalls};
}

test("protected composer disables controls and rejects keyboard, send and attachment events",async()=>{
  const {ctx,elements,apiCalls}=composer();
  ctx.initialize({getActive:()=>42,readOnly:()=>true});
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
