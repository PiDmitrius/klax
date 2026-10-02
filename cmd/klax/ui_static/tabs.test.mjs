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

function modelRefreshHarness(){
  const requests = [], notices = [], picks = [];
  let root, thinkRoot;
  const classes = () => {
    const names = new Set();
    return { contains: name => names.has(name), add: name => names.add(name), remove: name => names.delete(name) };
  };
  function makeRoot(id = "s-model"){
    const action = { attrs: {}, setAttribute(key, value){ this.attrs[key] = value; } };
    const menu = { classList: classes(), addEventListener(){}, querySelectorAll(selector){
      assert.equal(selector, ".sselect-opt[data-value]", "refresh action must not enter the model selection handler");
      return [];
    } };
    return {
      isConnected: true, classList: classes(), dataset: {}, action, menu,
      querySelector(selector){
        if(selector === '[data-action="refresh"]') return action;
        if(selector === ".sselect-menu") return menu;
        if(selector === ".sselect-btn") return { addEventListener(){} };
        throw new Error(selector);
      },
      set outerHTML(html){ this.isConnected = false; const next = makeRoot(id); next.html = html; if(id === "s-model") root = next; else thinkRoot = next; },
    };
  }
  root = makeRoot(); thinkRoot = makeRoot("s-think");
  const context = vm.createContext({
    document: { getElementById: id => id === "s-model" ? root : id === "s-think" ? thinkRoot : null, querySelectorAll: () => [] },
    esc: value => String(value),
    api(path, options){
      let resolve;
      const promise = new Promise(done => { resolve = done; });
      requests.push({ path, options, resolve });
      return promise;
    },
  });
  const source = readFileSync(new URL("./tabs.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
  vm.runInContext(source, context);
  context.onNotice = text => notices.push(text);
  context.onPick = value => picks.push(value);
  vm.runInContext("deps = { notice: onNotice };", context);
  function show(backend, selected, isDraft = false){
    root.isConnected = false; thinkRoot.isConnected = false;
    root = makeRoot(); thinkRoot = makeRoot("s-think"); root.classList.add("open");
    context.view = { backend, model: selected, models: [{ value: "old", label: "Old" }] };
    context.isDraft = isDraft;
    vm.runInContext('draftView = isDraft ? view : null; draft = isDraft ? { name: "unsaved", model: view.model } : null; wireModelSelect(view, isDraft, onPick);', context);
  }
  return { context, requests, notices, picks, show, root: () => root, thinkRoot: () => thinkRoot,
    run: code => vm.runInContext(code, context) };
}

test("model refresh is an action, preserves selection and draft, and saves no session settings", async () => {
  const h = modelRefreshHarness();
  h.show("codex", "pinned", true);
  let stopped = false;
  const pending = h.root().action.onclick({ stopPropagation(){ stopped = true; } });
  assert.equal(stopped, true);
  assert.equal(h.root().action.disabled, true);
  await h.root().action.onclick({ stopPropagation(){} });
  assert.equal(h.requests.length, 1);
  assert.equal(h.root().action.title, "Обновление…");
  assert.equal(h.requests[0].path, "/api/models/refresh");
  assert.deepEqual(JSON.parse(h.requests[0].options.body), { backend: "codex" });
  h.requests[0].resolve({ ok: true, json: async () => ({ models: [{ value: "new", label: "New" }] }) });
  await pending;
  assert.equal(h.context.view.model, "pinned");
  assert.equal(h.run("draft.name"), "unsaved");
  assert.equal(h.run("draftView.models[0].value"), "new");
  assert.match(h.root().html, /data-value="pinned"/);
  assert.match(h.root().html, /data-value="new"/);
  assert.match(h.root().html, /data-value=""><span>По умолчанию<\/span><button[^>]*data-action="refresh"/);
  assert.doesNotMatch(h.root().html, /ssep|\(обновить список\)/);
  assert.equal(h.root().action.disabled, false);
  assert.equal(h.root().action.attrs["aria-busy"], "false");
  assert.equal(h.root().classList.contains("open"), true);
  assert.deepEqual(h.picks, []);
  assert.equal(h.root().action.title, "Обновить список");
});

test("refresh response cannot replace another backend's menu", async () => {
  const h = modelRefreshHarness();
  h.show("codex", "pinned");
  const pending = h.root().action.onclick({ stopPropagation(){} });
  h.show("claude", "opus");
  h.requests[0].resolve({ ok: true, json: async () => ({ models: [{ value: "gpt-new", label: "New" }] }) });
  await pending;
  assert.equal(h.context.view.backend, "claude");
  assert.equal(h.context.view.models[0].value, "old");
  assert.equal(h.root().html, undefined);
});

test("refresh failures retain models and expose the reason", async () => {
  const h = modelRefreshHarness();
  h.show("claude", "opus");
  const pending = h.root().action.onclick({ stopPropagation(){} });
  h.requests[0].resolve({ ok: false, text: async () => "CLI unavailable" });
  await pending;
  assert.equal(h.context.view.model, "opus");
  assert.equal(h.context.view.models[0].value, "old");
  assert.deepEqual(h.notices, ["CLI unavailable"]);
  assert.equal(h.root().action.title, "Обновить список");
});

test("refresh uses the current selection after settings re-render", async () => {
  const h = modelRefreshHarness();
  h.show("codex", "first");
  const pending = h.root().action.onclick({ stopPropagation(){} });
  h.show("codex", "second");
  assert.equal(h.root().action.title, "Обновление…");
  h.requests[0].resolve({ ok: true, json: async () => ({ models: [{ value: "new", label: "New" }] }) });
  await pending;
  assert.equal(h.context.view.model, "second");
  assert.match(h.root().html, /data-value="second"/);
  assert.equal(h.context.view.models[0].value, "new");
});

test("efforts follow model capabilities, including default and missing metadata", () => {
  const h = modelRefreshHarness();
  h.context.models = [
    { value: "a", default: true, efforts: ["low", "high", "ultra"] },
    { value: "b", efforts: ["high", "max"] },
    { value: "c" },
  ];
  for(const [model, expected] of [["", ["low", "high", "ultra"]], ["b", ["high", "max"]], ["c", []], ["unknown", []]]){
    h.context.selected = model;
    assert.equal(h.run('JSON.stringify(modelEfforts({models, model: selected}).map(e => e.value))'), JSON.stringify(expected));
  }
  h.run('draft = { model: "a", think: "ultra" }; draftView = {models}; renderDraft = () => {}; draftApply({ model: "b" });');
  assert.equal(h.run('draft.think'), "");
  h.run('draft.think = "high"; draftApply({ model: "a" });');
  assert.equal(h.run('draft.think'), "high");
});

test("catalog refresh updates effort options without changing selection", async () => {
  const h = modelRefreshHarness();
  h.show("codex", "pinned", true);
  h.context.view.think = "ultra";
  const pending = h.run('refreshModels("codex")');
  h.requests[0].resolve({ok: true, json: async () => ({models: [{value: "pinned", label: "pinned", efforts: ["low", "high"]}]})});
  await pending;
  assert.equal(h.run('JSON.stringify(view.efforts.map(e => e.value))'), '["low","high"]');
  assert.equal(h.context.view.think, "ultra");
  assert.equal(h.run('draftView.models[0].efforts[0]'), "low");
  assert.equal(h.picks.length, 0);
});

test("either catalog button locks both and refreshes both menus, keeping effort menu open", async () => {
  const h = modelRefreshHarness();
  h.show("codex", "pinned");
  h.root().classList.remove("open");
  h.thinkRoot().classList.add("open");
  h.context.view.think = "high";
  const pending = h.thinkRoot().action.onclick({stopPropagation(){}});
  assert.equal(h.root().action.disabled, true);
  assert.equal(h.thinkRoot().action.disabled, true);
  h.root().action.onclick({stopPropagation(){}});
  assert.equal(h.requests.length, 1);
  h.requests[0].resolve({ok: true, json: async () => ({models: [{value: "pinned", label: "pinned", efforts: ["high", "max"]}]})});
  await pending;
  assert.equal(h.root().action.disabled, false);
  assert.equal(h.thinkRoot().action.disabled, false);
  assert.equal(h.thinkRoot().classList.contains("open"), true);
  assert.equal(h.root().classList.contains("open"), false);
  assert.match(h.thinkRoot().html, /data-value="max"/);
  assert.match(h.thinkRoot().html, /data-value=""><span>По умолчанию<\/span><button[^>]*data-action="refresh"/);
  assert.equal(h.context.view.think, "high");
  assert.equal(h.picks.length, 0);
});
