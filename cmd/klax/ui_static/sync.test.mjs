import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { TurnModel, ordLess, applyMerge } from "./model.js";
import { cursorEpoch, cursorSeq } from "./events.js";
import { apiError, retryDelay, syncInterval } from "./base.js";
globalThis.document = { getElementById: () => ({ textContent: JSON.stringify({request_ms:10000,poll_ms:30000,retry_min_ms:625,retry_max_ms:5000,sync_ms:250}) }) };
import { pos, parsePos, decodePos, answerBlock } from "./render.js";

function harness(epoch = null){
  const requests = [];
  const notices = [], homes = [];
  const stored = new Map(epoch === null ? [] : [["klax_server_epoch", epoch]]);
  const context = vm.createContext({
    TurnModel, ordLess, applyMerge, apiError, retryDelay, syncInterval, cursorEpoch, cursorSeq, pos, parsePos, decodePos, answerBlock, console, setTimeout, clearTimeout, AbortController,
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    api: url => new Promise(resolve => requests.push({ url, resolve })),
    selectionInLog: () => false, isReadOnly: () => true, filterScope: l => l, parseHash: () => ({}),
    reconcileSessions() {}, renderChip() {}, showNotice: value => notices.push(value), systemRestartNotice: (kind, version) => ({ kind, version }),
    writeHash() {}, storageKey: () => "session", loadDraft() {}, saveDraft() {}, setHome: value => homes.push(value),
    sessionStorage: { getItem: key => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, value) },
    document: { visibilityState: "visible", getElementById: () => null },
  });
  const source = readFileSync(new URL("./app.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "")
    .split('\napplyTheme((() =>')[0];
  vm.runInContext(source + `
    sessionList = [{ klax_id: "1" }];
    after = "1.10";
    refreshStrip = () => {};
    showTranscriptStatus = () => {};
    rerenderStructural = () => {};
    host.onAffected = () => {};
    setEmptyScope = () => {};
    globalThis.ensureLineLoadedActual = ensureLineLoaded;
    ensureLineLoaded = async () => {};
    globalThis.seqs = () => model.turns("1").map(t => t.turn_seq);
    globalThis.blocks = seq => model.turns("1").find(t => t.turn_seq === seq).blocks.map(b => b.id);
  `, context);
  const respond = (i, body, ok = true) => {
    if(requests[i].url === "/api/state" && ok) body = { system: { startup: "started", version: "0.9.0", uptime_sec: 0, home: "/home/<user>" }, ...body };
    requests[i].resolve({ ok, status: ok ? 200 : 503, json: async () => body });
  };
  return { run: code => vm.runInContext(code, context), requests, respond, notices, homes, stored };
}

const group = (seq, ids, state = "done") => ({ key: "t:" + seq, ord: seq + "." + seq, head: { turn_seq: seq, role: "user", state }, blocks: ids.map(id => ({ id })) });

test("live retries preserve the cursor, cap their pauses and reset after successful application", async () => {
  const pauses = [], cursors = [], health = [];
  let call = 0, after = "epoch.7", applied = 0;
  const context = {
    retryDelay,
    setTimeout: (fn, ms) => { pauses.push(ms); queueMicrotask(fn); },
    api: async (_url, opts, longPoll) => {
      assert.equal(longPoll, true);
      cursors.push(JSON.parse(opts.body).after);
      if(call++ < 5 || call === 7) throw new Error("offline");
      if(call === 8) return { status: 401 };
      return { ok: true, json: async () => ({ at: "epoch.8", events: [{ seq: 8 }] }) };
    },
    host: {
      after: () => after,
      apply: (_events, at) => { applied++; after = at; },
      onHealth: (ok, failures) => health.push([ok, failures]),
    },
  };
  const source = readFileSync(new URL("./events.js", import.meta.url), "utf8").replace(/^import .*;\n/gm, "").replace(/export /g, "");
  await vm.runInNewContext(source + "\nchangesLoop(host)", context);
  assert.deepEqual(pauses, [625, 1250, 2500, 5000, 5000, 625]);
  assert.deepEqual(cursors, ["epoch.7", "epoch.7", "epoch.7", "epoch.7", "epoch.7", "epoch.7", "epoch.8", "epoch.8"]);
  assert.equal(applied, 1);
  assert.deepEqual(health.at(-2), [true, 0]);
  assert.deepEqual(health.at(-1), [false, 1]);
});

for(const [name, previous, uptime, startup, show] of [
  ["a new tab", null, 0, "installed", false],
  ["the same server epoch", "new", 0, "installed", false],
  ["a recent installation", "old", 299, "installed", true],
  ["a recent restart", "old", 299, "started", true],
  ["a startup exactly five minutes old", "old", 300, "installed", false],
  ["a startup one week old", "old", 604800, "started", false],
]){
  test("startup notice for " + name, async () => {
    const h = harness(previous);
    const boot = h.run("bootState()");
    h.respond(0, { at: "new.12", system: { startup, version: "0.9.10", uptime_sec: uptime, home: "/home/<user>" }, sessions: [] });
    await boot;
    assert.deepEqual(h.notices, show ? [{ kind: startup, version: "0.9.10" }] : []);
    assert.deepEqual(h.homes, ["/home/<user>"]);
    assert.equal(h.run("serverEpoch"), "new");
    assert.equal(h.stored.get("klax_server_epoch"), "new");
    assert.equal(h.run("after"), "new.12");
  });
}

test("a suppressed startup notice still adopts the epoch across reloads", async () => {
  const h = harness("old");
  const first = h.run("bootState()");
  h.respond(0, { at: "new.12", system: { startup: "installed", version: "0.9.10", uptime_sec: 604800 }, sessions: [] });
  await first;
  const reloaded = harness(h.stored.get("klax_server_epoch"));
  const second = reloaded.run("bootState()");
  reloaded.respond(0, { at: "new.13", sessions: [] });
  await second;
  assert.deepEqual(h.notices, []);
  assert.deepEqual(reloaded.notices, []);
});

test("events during a window load are buffered; those the window already holds are skipped", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.run(`applyEvents([
    { seq: 11, klax_id: "1", group: { key: "t:5", ord: "5.5", blocks: { start: 1, append: [{ id: "b" }] } } },
    { seq: 13, klax_id: "1", group: { key: "t:6", ord: "6.6", create: { head: { turn_seq: 6, role: "user", state: "enq" }, blocks: [] } } },
  ], "1.13")`);
  assert.deepEqual(h.run("seqs()"), []);
  h.respond(0, { at: "1.12", from: "5.5", to: null, more: false, groups: [group(5, ["a", "b"])] });
  await load;
  assert.deepEqual(Array.from(h.run("seqs()")), [5, 6]);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
  h.run(`applyEvents([{ seq: 14, klax_id: "1", group: { key: "t:5", ord: "5.5", blocks: { start: 2, append: [{ id: "c" }] } } }], "1.14")`);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b", "c"]);
});

test("a page load keeps applying events to the held tail and buffers only its own range", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: "5.5", to: null, more: true, groups: [group(5, ["a"], "run")] });
  await load;
  const page = h.run("loadOlder('1')");
  assert.match(h.requests[1].url, /to=5\.5/);
  h.run(`applyEvents([
    { seq: 11, klax_id: "1", group: { key: "t:5", ord: "5.5", blocks: { start: 1, append: [{ id: "b" }] } } },
    { seq: 12, klax_id: "1", removed: { key: "t:3", ord: "3.3" } },
  ], "1.12")`);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
  h.respond(1, { at: "1.11", from: "2.2", to: "5.5", more: false, groups: [group(2, []), group(3, [])] });
  await page;
  assert.deepEqual(Array.from(h.run("seqs()")), [2, 5]);
});

test("a delta for a group the session should hold but lacks reloads its window", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: "5.5", to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run(`applyEvents([
    { seq: 11, klax_id: "2", group: { key: "t:7", ord: "7.7", blocks: { start: 1, append: [{ id: "x" }] } } },
    { seq: 12, klax_id: "1", group: { key: "t:7", ord: "7.7", blocks: { start: 1, append: [{ id: "x" }] } } },
  ], "1.12")`);
  assert.equal(h.requests.length, 2);
  assert.match(h.requests[1].url, /klax_id=1&/);
  assert.match(h.requests[1].url, /limit=/);
});

test("a page that finishes after a newer window cannot roll it back", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: "5.5", to: null, more: true, groups: [group(5, ["a"], "run")] });
  await load;
  const page = h.run("loadOlder('1')");
  h.run("reloadWindow('1')");
  h.run(`applyEvents([{ seq: 11, klax_id: "1", group: { key: "t:5", ord: "5.5", blocks: { start: 1, append: [{ id: "b" }] } } }], "1.11")`);
  h.respond(2, { at: "1.12", from: "5.5", to: null, more: true, groups: [group(5, ["a", "b", "c"])] });
  await new Promise(resolve => setImmediate(resolve));
  h.run(`applyEvents([{ seq: 12, klax_id: "1", group: { key: "t:5", ord: "5.5", blocks: { start: 2, append: [{ id: "c" }] } } }], "1.12")`);
  h.respond(1, { at: "1.10", from: "2.2", to: "5.5", more: false, groups: [group(2, [])] });
  await page;
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b", "c"]);
});

test("a failed snapshot during resync keeps the sessions to restore", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  const first = h.run("resync()");
  h.respond(1, {}, false);
  await assert.rejects(first);
  const second = h.run("resync()");
  h.respond(2, { at: "2.1", sessions: [{ klax_id: "1" }] });
  await second;
  assert.match(h.requests[3].url, /transcript\?klax_id=1&/);
});

test("a newer window that needs older history pages it in while a superseded page is still in flight", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: "5.5", to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("moreFor[1] = true; ensureLineLoaded = ensureLineLoadedActual");
  const page = h.run("loadOlder('1')");
  h.run("reloadWindow('1')");
  h.respond(2, { at: "1.11", from: "5.5", to: null, more: true, groups: [group(5, ["a"])] });
  for(let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.requests.length, 4);
  assert.match(h.requests[3].url, /to=5\.5/);
  h.respond(1, { at: "1.10", from: "2.2", to: "5.5", more: false, groups: [] });
  h.respond(3, { at: "1.11", from: null, to: "5.5", more: false, groups: [] });
  await page;
});

test("a resync raises a kept read watermark from the snapshot", async () => {
  const h = harness();
  h.run("parsePos = s => s === '9.0' ? 9e6 : 1e6");
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  assert.equal(h.run("readThrough[1]"), 1e6);
  const sync = h.run("resync()");
  h.respond(1, { at: "2.1", sessions: [{ klax_id: "1", read_pos: "9.0" }] });
  await sync;
  assert.equal(h.run("readThrough[1]"), 9e6);
});

test("published read positions confirm local progress without another report", async () => {
  const h = harness();
  h.run("active = '1'; readThrough[1] = pos(2, 3)");
  for(const read_pos of ["2.3", "3.0", "1.0"]){
    await h.run(`onSessionsList([{ klax_id: "1", read_pos: "${read_pos}" }])`);
    h.run("flushRead('1')");
    assert.equal(h.requests.length, 0);
  }
  assert.equal(h.run("readThrough[1]"), pos(3, 0));
});

test("a published read position stops a pending retry before its deadline", async () => {
  const h = harness();
  h.run(`
    globalThis.readTimers = new Map();
    setTimeout = (fn, ms) => { const id = Symbol(); readTimers.set(id, { fn, ms }); return id; };
    clearTimeout = id => readTimers.delete(id);
    active = "1"; readThrough[1] = pos(2, 3); flushRead("1");
  `);
  h.respond(0, null, false);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.run("readTimers.size"), 1);
  assert.equal(h.run("[...readTimers.values()][0].ms"), 625);
  await h.run('onSessionsList([{ klax_id: "1", read_pos: "2.3" }])');
  assert.equal(h.run("readTimers.size"), 0);
  h.run('flushRead("1")');
  assert.equal(h.requests.length, 1);
});

test("tab patches and orders rebuild the strip from the snapshot", async () => {
  const h = harness();
  h.run(`
    tabs = new Map([["1", { klax_id: "1", name: "one", unread: 3 }], ["2", { klax_id: "2", name: "two" }]]);
    tabOrder = ["1", "2"];
    globalThis.strips = [];
    onSessionsList = async list => { strips.push(JSON.parse(JSON.stringify(list))); };
    applyEvents([
      { seq: 11, klax_id: "1", tab: { unread: null, read_pos: "2.0" } },
      { seq: 12, klax_id: "3", tab: { name: "three" } },
      { seq: 13, tabs: ["3", "1"] },
    ], "1.13");
  `);
  assert.equal(h.run("JSON.stringify(strips)"), JSON.stringify([[{ klax_id: "3", name: "three" }, { klax_id: "1", name: "one", read_pos: "2.0" }]]));
  assert.equal(h.run("tabs.has('2')"), false);
});

test("a resync refreshes a held session in place", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: "5.5", to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("loaded[1] = true");
  const sync = h.run("resync()");
  h.respond(1, { at: "2.1", sessions: [{ klax_id: "1" }] });
  await sync;
  assert.equal(h.run("loaded[1] && model.has('1')"), true);
  assert.match(h.requests[2].url, /transcript\?klax_id=1&limit=/);
  h.respond(2, { at: "2.2", from: "5.5", to: null, more: false, groups: [group(5, ["a", "b"])] });
  for(let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.run("loaded[1]"), true);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
});

test("a refresh keeps the old view when paging back fails, then swaps and renders once", async () => {
  const h = harness();
  h.run("globalThis.renders = 0; rerenderStructural = () => { renders++; }");
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(2, []), group(5, ["a"])] });
  await load;
  h.run("renders = 0; loaded[1] = true");
  const failed = h.run("refreshWindow('1')");
  h.respond(1, { at: "1.11", from: "5.5", more: true, groups: [group(5, ["a"])] });
  for(let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
  assert.match(h.requests[2].url, /to=5\.5/);
  h.respond(2, {}, false);
  await failed;
  assert.deepEqual(Array.from(h.run("seqs()")), [2, 5]);
  assert.equal(h.run("renders"), 0);
  const retry = h.run("refreshWindow('1', 1)");
  h.respond(3, { at: "1.12", from: "5.5", more: true, groups: [group(5, ["a", "b"])] });
  for(let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve));
  h.respond(4, { at: "1.12", from: null, more: false, groups: [group(2, [])] });
  await retry;
  assert.deepEqual(Array.from(h.run("seqs()")), [2, 5]);
  assert.deepEqual(Array.from(h.run("blocks(5)")), ["a", "b"]);
  assert.equal(h.run("renders"), 1);
  assert.equal(h.run("refreshing[1]"), undefined);
});

test("switching to a tab whose window is being refreshed shows loading until it is done", async () => {
  const h = harness();
  h.run(`globalThis.statuses = []; showTranscriptStatus = (m = '') => statuses.push(m); sessionList = [{ klax_id: "1" }, { klax_id: "2" }]`);
  const load = h.run("loadTranscript('2')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("loaded[2] = true; active = '1'");
  const refresh = h.run("refreshWindow('2')");
  await h.run("selectSession('2')");
  assert.equal(h.run("statuses.at(-1)"), "Загрузка истории…");
  await h.run("selectSession('2')");
  assert.equal(h.run("statuses.at(-1)"), "Загрузка истории…");
  h.respond(1, { at: "1.11", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await refresh;
  assert.equal(h.run("statuses.at(-1)"), "");
});

test("the visible message keeps its offset when history grows above it", () => {
  const h = harness();
  h.run(`
    globalThis.fake = { shift: 0 };
    const node = (key, top) => ({ dataset: { renderKey: key }, getBoundingClientRect: () => ({ top: top + fake.shift - log.scrollTop, bottom: top + 50 + fake.shift - log.scrollTop }) });
    const log = { scrollTop: 300, getBoundingClientRect: () => ({ top: 0 }) };
    const col = { children: [node("turn:1", 0), node("turn:2", 280), node("turn:3", 400)] };
    document.getElementById = id => id === "log" ? log : col;
    globalThis.fakeLog = log;
  `);
  const anchor = h.run("viewAnchor()");
  assert.equal(anchor.key, "turn:2");
  h.run("fake.shift = 700; restoreAnchor(" + JSON.stringify(anchor) + ")");
  assert.equal(h.run("fakeLog.scrollTop"), 1000);
});

test("a long-failing refresh keeps retrying with a bounded pause", async () => {
  const h = harness();
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("loaded[1] = true; globalThis.delays = []; setTimeout = (fn, ms) => { delays.push(ms); return 0; }; showNotice = () => {}");
  const refresh = h.run("refreshWindow('1', 25)");
  h.respond(1, {}, false);
  await refresh;
  assert.equal(h.run("delays.at(-1)"), 5000);
  assert.equal(h.run("model.has('1') && loaded[1]"), true);
});

test("closing the active session stops its pending refresh", async () => {
  const h = harness();
  h.run(`sessionList = [{ klax_id: "1" }, { klax_id: "2" }]`);
  const load = h.run("loadTranscript('1')");
  h.respond(0, { at: "1.10", from: null, to: null, more: false, groups: [group(5, ["a"])] });
  await load;
  h.run("loaded[1] = true; active = '1'; globalThis.timers = []; setTimeout = fn => { timers.push(fn); return 0; }; neighborIn = () => 0; markRead = () => {}; dropDraft = () => {}");
  const refresh = h.run("refreshWindow('1')");
  h.respond(1, {}, false);
  await refresh;
  await h.run(`onSessionsList([{ klax_id: "2" }])`);
  h.run("timers.forEach(fn => fn())");
  assert.deepEqual(h.requests.slice(2).map(r => r.url).filter(u => /klax_id=1/.test(u)), []);
  assert.equal(h.run("model.has('1') || !!refreshing[1] || !!loading[1]"), false);
});
