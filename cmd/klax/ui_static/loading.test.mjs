import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { TurnModel } from "./model.js";

function harness(){
  const requests = [], painted = [], statuses = [], focused = [];
  const context = vm.createContext({
    TurnModel, console, setTimeout, clearTimeout,
    api: url => new Promise(resolve => requests.push({ url, resolve })),
    parsePos: () => 0,
    selectionInLog: () => false,
    writeHash() {}, storageKey: () => "session",
    loadDraft() {}, saveDraft() {},
    document: { visibilityState: "visible", getElementById: () => null },
    painted, statuses, focused,
  });
  const source = readFileSync(new URL("./app.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "")
    .split('\napplyTheme((() =>')[0];
  vm.runInContext(source + `
    sessionList = [1, 2, 3].map(created => ({created}));
    loaded[1] = true;
    rememberScroll = () => {};
    restoreScroll = () => {};
    refreshStrip = () => painted.push(["tab", active]);
    focusComposer = () => focused.push(active);
    showTranscriptStatus = (message = "", retry = false) => statuses.push([active, message, retry]);
    rerenderStructural = created => { if(created === active && loaded[created]) painted.push(["log", created]); };
    globalThis.select = selectSession;
    globalThis.ready = created => !!loaded[created];
    globalThis.read = created => readThrough[created];
    globalThis.setSessions = ids => { sessionList = ids.map(created => ({created})); };
  `, context);
  const respond = (index, { ok = true, more = false, offset = 0 } = {}) => {
    requests[index].resolve({ ok, status: ok ? 200 : 503,
      json: async () => ({ turns: [], more, offset, read_through: "0.0" }) });
  };
  return { context, requests, painted, statuses, focused, respond };
}

test("cold selection responds immediately, shares its request, and finishes offscreen", async () => {
  const h = harness();
  const first = h.context.select(2);
  assert.equal(h.requests.length, 1);
  assert.deepEqual(Array.from(h.painted.at(-1)), ["tab", 2]);
  assert.equal(h.statuses.at(-1)[1], "Загрузка истории…");
  const again = h.context.select(2);
  assert.equal(h.requests.length, 1);
  await h.context.select(1);
  const paints = h.painted.length, focus = h.focused.length, statuses = h.statuses.length;
  h.respond(0);
  await Promise.all([first, again]);
  assert.equal(h.context.ready(2), true);
  assert.equal(h.painted.length, paints);
  assert.equal(h.focused.length, focus);
  assert.equal(h.statuses.length, statuses);
  await h.context.select(2);
  assert.equal(h.requests.length, 1);
  assert.deepEqual(Array.from(h.painted.at(-1)), ["log", 2]);
});

test("failure offers retry and leaves the transcript unloaded", async () => {
  const h = harness();
  const first = h.context.select(2);
  h.respond(0, { ok: false });
  await first;
  assert.equal(h.context.ready(2), false);
  assert.equal(h.statuses.at(-1)[2], true);
  const retry = h.context.select(2);
  assert.equal(h.requests.length, 2);
  h.respond(1);
  await retry;
  assert.equal(h.context.ready(2), true);
  assert.equal(h.statuses.at(-1)[1], "");
});

test("returning to a loading tab waits for the same history and then displays it", async () => {
  const h = harness();
  const first = h.context.select(2);
  h.respond(0, { more: true, offset: 20 });
  await new Promise(resolve => setImmediate(resolve));
  await h.context.select(1);
  const back = h.context.select(2);
  assert.equal(h.requests.length, 2);
  assert.equal(h.statuses.at(-1)[1], "Загрузка истории…");
  h.respond(1);
  await Promise.all([first, back]);
  assert.equal(h.context.ready(2), true);
  assert.equal(h.statuses.at(-1)[1], "");
  assert.deepEqual(Array.from(h.painted.at(-1)), ["log", 2]);
});

test("initial history pagination stays loading and surfaces a failed page", async () => {
  const h = harness();
  const first = h.context.select(2);
  h.respond(0, { more: true, offset: 20 });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.requests.length, 2);
  assert.equal(h.context.ready(2), false);
  await h.context.select(1);
  assert.equal(h.context.read(2), 0);
  const statuses = h.statuses.length;
  h.respond(1, { ok: false });
  await first;
  assert.equal(h.context.ready(2), false);
  assert.equal(h.statuses.length, statuses);
});

test("closing a session during initial pagination cannot restore it", async () => {
  const h = harness();
  const first = h.context.select(2);
  h.respond(0, { more: true, offset: 20 });
  await new Promise(resolve => setImmediate(resolve));
  await h.context.select(1);
  h.context.setSessions([1, 3]);
  h.respond(1);
  await first;
  assert.equal(h.context.ready(2), false);
});
