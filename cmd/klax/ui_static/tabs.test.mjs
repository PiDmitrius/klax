import { test } from "node:test";
import assert from "node:assert/strict";
import { reconcileSessions, renderTabs } from "./tabs.js";

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
