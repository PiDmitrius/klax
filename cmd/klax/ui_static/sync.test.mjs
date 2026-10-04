import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { TurnModel, ordLess, ordParam } from "./model.js";
import { cursorEpoch, cursorSeq } from "./events.js";
import { pos, answerBlock } from "./render.js";

function harness(){
  const requests = [];
  const context = vm.createContext({
    TurnModel, ordLess, ordParam, cursorEpoch, cursorSeq, pos, answerBlock, console, setTimeout, clearTimeout,
    api: url => new Promise(resolve => requests.push({ url, resolve })),
    parsePos: () => 0, selectionInLog: () => false, isReadOnly: () => true, filterScope: l => l, parseHash: () => ({}),
    reconcileSessions() {}, renderChip() {}, showNotice() {}, systemRestartNotice: () => "",
    writeHash() {}, storageKey: () => "session", loadDraft() {}, saveDraft() {},
    document: { visibilityState: "visible", getElementById: () => null },
  });
  const source = readFileSync(new URL("./app.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "")
    .split('\napplyTheme((() =>')[0];
  vm.runInContext(source + `
    sessionList = [{ created: 1 }];
    after = "1.10";
    refreshStrip = () => {};
    showTranscriptStatus = () => {};
    rerenderStructural = () => {};
    host.onAffected = () => {};
    setEmptyScope = () => {};
    globalThis.ensureLineLoadedActual = ensureLineLoaded;
    ensureLineLoaded = async () => {};
    globalThis.seqs = () => model.turns(1).map(t => t.seq);
    globalThis.blocks = seq => model.turns(1).find(t => t.seq === seq).blocks.map(b => b.id);
  `, context);
  const respond = (i, body, ok = true) => requests[i].resolve({ ok, status: ok ? 200 : 503, json: async () => body });
  return { run: code => vm.runInContext(code, context), requests, respond };
}

const group = (seq, ids, state = "done") => ({ key: "t:1:" + seq, ord: [seq, seq], head: { seq, role: "user", state }, blocks: ids.map(id => ({ id })) });

test("events during a window load are buffered; those the window already holds are skipped", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.run(`applyEvents([
    { seq: 11, session: 1, group: { key: "t:1:5", ord: [5, 5], n: 2, from: 1, blocks: [{ id: "b" }] } },
    { seq: 13, session: 1, group: { key: "t:1:6", ord: [6, 6], head: { seq: 6, role: "user", state: "enq" }, n: 0, from: 0, blocks: [] } },
  ], "1.13")`);
  assert.deepEqual(h.run("seqs()"), []);
  h.respond(0, { at: "1.12", from: [5, 5], to: null, more: false, groups: [group(5, ["a", "b"])] });
  await load;
  assert.deepEqual(Array.from(h.run("seqs()")), [5, 6]);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
  h.run(`applyEvents([{ seq: 14, session: 1, group: { key: "t:1:5", ord: [5, 5], n: 3, from: 2, blocks: [{ id: "c" }] } }], "1.14")`);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b", "c"]);
});

test("a page load keeps applying events to the held tail and buffers only its own range", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.respond(0, { at: "1.10", from: [5, 5], to: null, more: true, groups: [group(5, ["a"], "run")] });
  await load;
  const page = h.run("loadOlder(1)");
  assert.match(h.requests[1].url, /before=5%2C5/);
  h.run(`applyEvents([
    { seq: 11, session: 1, group: { key: "t:1:5", ord: [5, 5], n: 2, from: 1, blocks: [{ id: "b" }] } },
    { seq: 12, session: 1, removed: { key: "t:1:3", ord: [3, 3] } },
  ], "1.12")`);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
  h.respond(1, { at: "1.11", from: [2, 2], to: [5, 5], more: false, groups: [group(2, []), group(3, [])] });
  await page;
  assert.deepEqual(Array.from(h.run("seqs()")), [2, 5]);
});

test("a delta for a group the session should hold but lacks reloads its window", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.respond(0, { at: "1.10", from: [5, 5], to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run(`applyEvents([
    { seq: 11, session: 2, group: { key: "t:2:7", ord: [7, 7], n: 2, from: 1, blocks: [{ id: "x" }] } },
    { seq: 12, session: 1, group: { key: "t:1:7", ord: [7, 7], n: 2, from: 1, blocks: [{ id: "x" }] } },
  ], "1.12")`);
  assert.equal(h.requests.length, 2);
  assert.match(h.requests[1].url, /session=1&/);
  assert.match(h.requests[1].url, /limit=/);
});

test("a page that finishes after a newer window cannot roll it back", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.respond(0, { at: "1.10", from: [5, 5], to: null, more: true, groups: [group(5, ["a"], "run")] });
  await load;
  const page = h.run("loadOlder(1)");
  h.run("reloadWindow(1)");
  h.run(`applyEvents([{ seq: 11, session: 1, group: { key: "t:1:5", ord: [5, 5], n: 2, from: 1, blocks: [{ id: "b" }] } }], "1.11")`);
  h.respond(2, { at: "1.12", from: [5, 5], to: null, more: true, groups: [group(5, ["a", "b", "c"])] });
  await new Promise(resolve => setImmediate(resolve));
  h.run(`applyEvents([{ seq: 12, session: 1, group: { key: "t:1:5", ord: [5, 5], n: 3, from: 2, blocks: [{ id: "c" }] } }], "1.12")`);
  h.respond(1, { at: "1.10", from: [2, 2], to: [5, 5], more: false, groups: [group(2, [])] });
  await page;
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b", "c"]);
});

test("a failed snapshot during resync keeps the sessions to restore", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.respond(0, { at: "1.10", from: [-1, 0], to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  const first = h.run("resync()");
  h.respond(1, {}, false);
  await assert.rejects(first);
  const second = h.run("resync()");
  h.respond(2, { at: "2.1", started: 2, sessions: [{ created: 1 }] });
  await second;
  assert.match(h.requests[3].url, /transcript\?session=1&/);
});

test("a newer window that needs older history pages it in while a superseded page is still in flight", async () => {
  const h = harness();
  const load = h.run("loadTranscript(1)");
  h.respond(0, { at: "1.10", from: [5, 5], to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("moreFor[1] = true; ensureLineLoaded = ensureLineLoadedActual");
  const page = h.run("loadOlder(1)");
  h.run("reloadWindow(1)");
  h.respond(2, { at: "1.11", from: [5, 5], to: null, more: true, groups: [group(5, ["a"])] });
  for(let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.requests.length, 4);
  assert.match(h.requests[3].url, /before=5%2C5/);
  h.respond(1, { at: "1.10", from: [2, 2], to: [5, 5], more: false, groups: [] });
  h.respond(3, { at: "1.11", from: [-1, 0], to: [5, 5], more: false, groups: [] });
  await page;
});
