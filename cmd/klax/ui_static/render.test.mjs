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
  querySelector(sel) {
    if(sel !== ".stop" || !this.html.includes('class="stop"')) return null;
    return this.stop ||= { disabled: false, addEventListener(type, fn) { this.click = fn; } };
  }
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

test("queued ✕ stops only its own turn and re-enables after a failed cancel", async () => {
  const h = harness();
  const calls = [];
  let result = false;
  const onStop = (state, seq) => { calls.push([state, seq]); return Promise.resolve(result); };
  h.render(h.col, [
    { seq: 1, role: "user", text: "running", state: "run", blocks: [] },
    { seq: 2, role: "user", text: "queued", state: "enq", blocks: [] },
  ], undefined, onStop);
  const [runDots, queuedDots] = h.col.children.map(turn => turn.children.find(c => c.dataset.flip === "dots"));
  assert.match(runDots.html, /title="Прервать"/);
  assert.match(queuedDots.html, /title="Убрать из очереди"/);

  queuedDots.stop.click();
  assert.equal(queuedDots.stop.disabled, true);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(queuedDots.stop.disabled, false);

  result = true;
  queuedDots.stop.click();
  runDots.stop.click();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(calls, [["enq", 2], ["enq", 2], ["run", 1]]);
  assert.equal(queuedDots.stop.disabled, true);
});

test("a cancelled note neither drives read-advance nor opens the unread divider", () => {
  const h = harness();
  h.render(h.col, [
    { seq: 1, role: "user", text: "running", state: "run", blocks: [{ id: "a", role: "assistant", text: "one" }] },
    { seq: 2, role: "user", text: "dropped", state: "err", blocks: [{ id: "c", role: "system", kind: "cancelled", text: "Отменено" }] },
  ], h.pos(1, 0));
  const [, note] = h.col.children[1].children;
  assert.equal(note.className.split(" ").includes("cancelled"), true);
  assert.equal(note.dataset.pos, undefined);
  assert.equal(h.col.children.flatMap(t => t.children).some(c => c.className === "readline"), false);
});
