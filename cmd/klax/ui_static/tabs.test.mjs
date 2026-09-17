import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { reconcileSessions, renderTabs } from "./tabs.js";
import { ROOT, setScope } from "./scope.js";

class Element {
  children = [];
  dataset = {};
  parentNode = null;
  classList = { toggle() {} };
  parts = new Map();
  addEventListener() {}
  querySelector(selector) {
    if(!this.parts.has(selector)) this.parts.set(selector, new Element());
    return this.parts.get(selector);
  }
  querySelectorAll() { return this.children.slice(); }
  remove() {
    if(this.parentNode) {
      const siblings = this.parentNode.children;
      siblings.splice(siblings.indexOf(this), 1);
      this.parentNode = null;
    }
  }
  insertBefore(node, ref) {
    if(node === ref) return node;
    node.remove();
    const index = ref == null ? this.children.length : this.children.indexOf(ref);
    assert.ok(index >= 0);
    this.children.splice(index, 0, node);
    node.parentNode = this;
    return node;
  }
  appendChild(node) { return this.insertBefore(node, null); }
}

test("strip resize preserves manual browsing and centering yields to a drag", () => {
  let resized;
  const scrolls = [];
  const tab = { getBoundingClientRect: () => ({ left: 110, width: 100 }) };
  const strip = {
    scrollLeft: 470, clientLeft: 0, clientWidth: 300, scrollWidth: 1000,
    addEventListener() {},
    querySelector: () => tab,
    getBoundingClientRect: () => ({ left: 80, width: 300 }),
    scrollTo(options) { scrolls.push(options); this.scrollLeft = options.left; },
  };
  const flags = new Map();
  const wrap = { classList: { toggle: (name, on) => flags.set(name, on) } };
  const context = vm.createContext({
    document: {
      getElementById: id => id === "tabs" ? strip : id === "tabswrap" ? wrap : null,
      querySelector: () => null, addEventListener() {},
    },
    ResizeObserver: class {
      constructor(callback) { resized = callback; }
      observe(element) { assert.equal(element, strip); }
    },
  });
  const source = readFileSync(new URL("./tabs.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
  vm.runInContext(source + "\ninitTabs({});", context);
  strip.clientWidth = 280;
  resized();
  assert.equal(strip.scrollLeft, 470);
  assert.equal(flags.get("overflow-left"), true);
  assert.equal(flags.get("overflow-right"), true);
  vm.runInContext("dragging = true; centerActiveTab(true);", context);
  resized();
  assert.equal(scrolls.length, 0);
  vm.runInContext("dragging = false; centerActiveTab(true);", context);
  assert.equal(strip.scrollLeft, 410);
  assert.equal(scrolls.length, 1);
});

test("scope changes reconcile tab order in one render and preserve retained nodes", () => {
  const strip = new Element();
  globalThis.document = {
    getElementById: id => id === "tabs" ? strip : null,
    createElement: () => new Element(),
    title: "klax",
  };
  globalThis.requestAnimationFrame = () => {};
  try {
    const sequences = [[1, 2, 3], [3], [1, 2, 3], [2, 3], [3, 2, 1], [], [4, 5], [1, 2, 3]];
    for(const ids of sequences) {
      const retained = new Map(strip.children.map(node => [node.dataset.created, node]));
      reconcileSessions(ids.map(created => ({ created })), ids.at(-1));
      assert.deepEqual(strip.children.map(node => Number(node.dataset.created)), ids);
      for(const node of strip.children) {
        const previous = retained.get(node.dataset.created);
        if(previous) assert.equal(node, previous);
      }
      const nodes = strip.children.slice();
      renderTabs(ids[0]);
      assert.deepEqual(strip.children, nodes);
    }
  } finally {
    delete globalThis.document;
    delete globalThis.requestAnimationFrame;
  }
});

test("active tabs center on entry and scope changes without overriding manual browsing on refresh", () => {
  const strip = new Element();
  Object.assign(strip, { scrollLeft: 0, clientLeft: 0, clientWidth: 300, scrollWidth: 1000 });
  strip.getBoundingClientRect = () => ({ left: 80, width: 300 });
  strip.querySelector = () => strip.children.find(t => t.className.includes(" active"));
  const scrolls = [];
  strip.scrollTo = options => { scrolls.push(options); strip.scrollLeft = options.left; };
  const frames = [];
  globalThis.requestAnimationFrame = fn => frames.push(fn);
  globalThis.document = {
    getElementById: id => id === "tabs" ? strip : null,
    createElement: () => {
      const tab = new Element();
      tab.getBoundingClientRect = () => ({
        left: 80 + strip.children.indexOf(tab) * 100 - strip.scrollLeft,
        width: 100,
      });
      return tab;
    },
    title: "klax",
  };
  const flush = () => { while(frames.length) frames.shift()(); };
  try {
    setScope(ROOT);
    reconcileSessions(Array.from({ length: 10 }, (_, i) => ({ created: 100 + i })), 105);
    flush();
    assert.equal(strip.scrollLeft, 400);

    strip.scrollLeft = 470;
    renderTabs(105);
    flush();
    assert.equal(strip.scrollLeft, 470);
    assert.equal(scrolls.length, 1);

    setScope({ kind: "group", name: "work" });
    renderTabs(105);
    flush();
    assert.equal(strip.scrollLeft, 400);

    renderTabs(100);
    flush();
    assert.equal(strip.scrollLeft, 0);
    renderTabs(109);
    flush();
    assert.equal(strip.scrollLeft, 700);

    // Rapid selections settle on the current DOM's active tab.
    renderTabs(102);
    renderTabs(106);
    flush();
    assert.equal(strip.scrollLeft, 500);
  } finally {
    setScope(ROOT);
    delete globalThis.document;
    delete globalThis.requestAnimationFrame;
  }
});
