import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

// A minimal DOM: elements with data attributes and classes, and text nodes.
class Node {
  constructor(tag, opts = {}){
    this.nodeType = tag === "#text" ? 3 : 1;
    this.data = opts.text || "";
    this.dataset = opts.dataset || {};
    this.cls = opts.cls || [];
    this.childNodes = [];
    this.parentElement = null;
    (opts.kids || []).forEach(k => this.append(k));
  }
  append(k){ k.parentElement = this; this.childNodes.push(k); return k; }
  get children(){ return this.childNodes.filter(c => c.nodeType === 1); }
  get classList(){ return { contains: c => this.cls.includes(c) }; }
  get isConnected(){ let n = this; while(n.parentElement) n = n.parentElement; return n === root; }
  contains(n){ for(; n; n = n.parentElement) if(n === this) return true; return false; }
  closest(sel){
    for(let n = this; n; n = n.parentElement){
      if(sel.startsWith(".") ? n.cls.includes(sel.slice(1)) : n.dataset.renderKey !== undefined) return n;
    }
    return null;
  }
}
const text = t => new Node("#text", { text: t });
const msg = (flip, ...parts) => new Node("div", { cls: ["msg"], dataset: { flip }, kids: [text("⧉"), new Node("div", { cls: ["body"], kids: parts.map(text) }), text("12:00")] });
const body = m => m.childNodes[1];
const turn = (key, ...msgs) => new Node("div", { dataset: { renderKey: key }, kids: msgs });
let root;

function harness(){
  const sel = {
    isCollapsed: false, rangeCount: 1,
    setBaseAndExtent(an, ao, fn, fo){ Object.assign(this, { anchorNode: an, anchorOffset: ao, focusNode: fn, focusOffset: fo }); this.set = true; },
    getRangeAt(){ return { commonAncestorContainer: this.anchorNode }; },
  };
  const context = vm.createContext({ document: { getSelection: () => sel } });
  vm.runInContext(readFileSync(new URL("./scroll.js", import.meta.url), "utf8").replace(/^export /gm, ""), context);
  return { sel, save: col => context.saveSelection(col), restore: (col, s) => context.restoreSelection(col, s) };
}

test("a selection survives its message being rebuilt with more text", () => {
  const h = harness();
  const old = msg("g:1", "Идёт ", "ответ");
  root = new Node("div", { kids: [turn("turn:1", msg("u", "вопрос"), old)] });
  const t = body(old).childNodes[1];
  h.sel.setBaseAndExtent(t, 3, t, 1);
  const saved = h.save(root);
  old.parentElement = null;
  root.childNodes[0].childNodes[1] = msg("g:1", "Идёт ответ", " и дальше");
  root.childNodes[0].childNodes[1].parentElement = root.childNodes[0];
  h.restore(root, saved);
  const now = body(root.childNodes[0].childNodes[1]).childNodes[0];
  assert.deepEqual([h.sel.anchorNode, h.sel.anchorOffset, h.sel.focusNode, h.sel.focusOffset], [now, 8, now, 6]);
});

test("an untouched selection and one ending outside the log are left as they are", () => {
  const h = harness();
  const m = msg("g:1", "текст");
  root = new Node("body");
  const col = root.append(new Node("div", { kids: [turn("turn:1", m)] }));
  const outside = root.append(text("снаружи"));
  const t = body(m).childNodes[0];
  h.sel.setBaseAndExtent(t, 1, outside, 2);
  h.sel.set = false;
  h.restore(col, h.save(col));
  assert.equal(h.sel.set, false);
  const saved = h.save(col);
  const moved = msg("g:1", "текст");
  col.childNodes[0].childNodes[0] = moved; moved.parentElement = col.childNodes[0];
  m.parentElement = null;
  h.restore(col, saved);
  assert.deepEqual([h.sel.anchorNode, h.sel.anchorOffset, h.sel.focusNode, h.sel.focusOffset], [body(moved).childNodes[0], 1, outside, 2]);
});

test("a selection in the second part of a split message survives the parts merging", () => {
  const h = harness();
  const first = msg("g:1000000", "начало "), second = msg("g:1000001", "продолжение");
  root = new Node("div", { kids: [turn("turn:1", msg("u", "вопрос"), first, new Node("div", { cls: ["readline"], kids: [text("новые")] }), second)] });
  const t = body(second).childNodes[0];
  h.sel.setBaseAndExtent(t, 2, t, 6);
  const saved = h.save(root);
  const merged = msg("g:1000000", "начало ", "продолжение");
  const tr = root.childNodes[0];
  [first, second].forEach(m => { m.parentElement = null; });
  tr.childNodes = [tr.childNodes[0]]; tr.append(merged);
  h.restore(root, saved);
  const now = body(merged).childNodes[1];
  assert.deepEqual([h.sel.anchorNode, h.sel.anchorOffset, h.sel.focusNode, h.sel.focusOffset], [now, 2, now, 6]);
});
