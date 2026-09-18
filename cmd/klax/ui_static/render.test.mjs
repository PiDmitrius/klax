import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { playShift } from "./render.js";

test("shift animates the visible tail of a tall bubble and skips fully distant bubbles", () => {
  for(const [top, bottom, expected] of [[100, 400, 180], [-3000, 400, 180], [-3000, -2000, 0], [2000, 2300, 0]]){
    const transforms = [];
    const el = {
      dataset: { renderKey: "tool" },
      classList: { contains: () => false },
      style: { set transform(value) { if(value) transforms.push(value); } },
      getBoundingClientRect: () => ({ top, bottom }),
      addEventListener() {},
    };
    const snap = { units: new Map([["tool", top + 30]]), keys: new Set(["tool"]), hadAny: true };
    assert.equal(playShift({ children: [el], offsetHeight: 4000 }, snap), expected);
    assert.deepEqual(transforms, expected ? ["translateY(30px)"] : []);
  }
});

class Element {
  children = [];
  dataset = {};
  className = "";
  writes = 0;
  classList = {
    contains: name => this.className.split(" ").includes(name),
    toggle: (name, on) => {
      const names = new Set(this.className.split(" ").filter(Boolean));
      if(on) names.add(name); else names.delete(name);
      this.className = [...names].join(" ");
    },
  };
  set innerHTML(value) { this.html = value; this.writes++; }
  get firstChild() { return this.children[0] || null; }
  get nextSibling() { return this.parent.children[this.parent.children.indexOf(this) + 1] || null; }
  insertBefore(node, ref) {
    if(node.parent) node.parent.removeChild(node);
    this.children.splice(ref ? this.children.indexOf(ref) : this.children.length, 0, node);
    node.parent = this;
  }
  removeChild(node) { this.children.splice(this.children.indexOf(node), 1); node.parent = null; }
  querySelectorAll() { return []; }
}

function harness(){
  const calls = { markdown: 0, escape: 0 };
  const context = vm.createContext({
    document: { createElement: () => new Element() },
    mdSafe: text => { calls.markdown++; return text; },
    esc: text => { calls.escape++; return text; },
    fmtDate: () => "", fmtTime: () => "",
  });
  const source = readFileSync(new URL("./render.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
  vm.runInContext(source, context);
  return { calls, render: context.renderSession, pos: context.pos, col: new Element() };
}

test("divider collapse and join preserve unchanged tool contents and skip text formatting", () => {
  const h = harness();
  const blocks = Array.from({ length: 301 }, (_, i) => ({ id: String(i), role: "tool", text: "tool " + i }));
  const turn = { seq: 1, role: "user", text: "request", state: "done", blocks };
  h.render(h.col, [turn], h.pos(1, 299));
  const container = h.col.children[0];
  const [user, tools, divider, tail] = container.children;
  assert.equal(divider.className, "readline");
  const counts = { ...h.calls };
  const held = new Map([[1, new Set([h.pos(1, 300)])]]);

  h.render(h.col, [turn], h.pos(1, 300), null, held);
  assert.deepEqual(container.children, [user, tools, tail]);
  h.render(h.col, [turn], h.pos(1, 300), null, held, true);
  assert.deepEqual(container.children, [user, tools, tail]);
  assert.equal(tools.classList.contains("join-next"), true);
  assert.equal(tail.classList.contains("join-prev"), true);
  assert.equal(tools.writes, 1);
  assert.equal(tail.writes, 1);
  assert.deepEqual(h.calls, counts);

  h.render(h.col, [turn], h.pos(1, 300));
  assert.deepEqual(container.children, [user, tools]);
  assert.equal(tools.classList.contains("join-next"), false);
  assert.equal(tools.writes, 2);
  assert.equal(tools._raw, blocks.map(b => b.text).join("\n"));
  assert.equal(tools.dataset.pos, String(h.pos(1, 300)));
  assert.equal(user.writes, 1);
  assert.equal(h.calls.markdown, 1);
});

test("content updates patch retained bubbles and join flags clear without rewriting content", () => {
  const h = harness();
  const turn = { seq: 1, role: "user", text: "request", state: "done", blocks: [
    { id: "a", role: "assistant", text: "one" },
    { id: "b", role: "assistant", text: "two" },
  ] };
  const held = new Map([[1, new Set([h.pos(1, 1)])]]);
  h.render(h.col, [turn], h.pos(1, 1), null, held, true);
  const [user, first, second] = h.col.children[0].children;
  turn.blocks[1].text = "updated";
  h.render(h.col, [turn], h.pos(1, 1), null, held, true);
  assert.deepEqual(h.col.children[0].children, [user, first, second]);
  assert.equal(second._raw, "updated");
  assert.equal(second.writes, 2);
  assert.equal(second.classList.contains("join-prev"), true);
  assert.equal(first.writes, 1);
  const counts = { ...h.calls };
  h.render(h.col, [turn], h.pos(1, 1), null, held);
  assert.equal(first.classList.contains("join-next"), false);
  assert.equal(second.classList.contains("join-prev"), false);
  assert.equal(second.writes, 2);
  assert.deepEqual(h.calls, counts);
});
