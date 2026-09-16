import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { TurnModel } from "./model.js";
import { pos } from "./render.js";

function harness(){
  let now = 0, nextTimer = 0;
  const timers = new Map(), calls = [];
  const log = { scrollTop: 1350, clientHeight: 600, scrollHeight: 2000 };
  const col = { offsetHeight: 2000 };
  const logEvents = {}, documentEvents = {};
  log.addEventListener = (name, fn) => { logEvents[name] = fn; };
  const arm = (fn, delay) => { const id = ++nextTimer; timers.set(id, { fn, at: now + delay }); return id; };
  const context = vm.createContext({
    TurnModel, calls, pos, fadeOutDivider: () => false,
    document: { visibilityState: "visible", getElementById: id => id === "log" ? log : col,
      addEventListener: (name, fn) => { documentEvents[name] = fn; } },
    setTimeout: arm, requestAnimationFrame: fn => arm(fn, 16),
    cancelAnimationFrame(id){ timers.delete(id); },
    clearTimeout(id){ timers.delete(id); },
  });
  const source = readFileSync(new URL("./app.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").split('\napplyTheme((() =>')[0];
  vm.runInContext(source + `
    active = 1; loaded[1] = loaded[2] = true; readThrough[1] = 0;
    globalThis.markReadActual = markRead;
    globalThis.commitLiveActual = commitLive;
    advanceReadThroughPastViewport = () => { calls.push("advance"); return true; };
    markRead = () => { calls.push("read"); return true; };
    capWindow = () => { calls.push("cap"); return 0; };
    refreshStrip = () => {};
    reportRead = () => {};
    rerenderStructural = () => calls.push("render");
    commitLive = () => calls.push("animate");
  `, context);
  function tick(ms){
    const end = now + ms;
    while(true){
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if(!due) break;
      now = due[1].at; timers.delete(due[0]); due[1].fn();
    }
    now = end;
  }
  return { log, col, calls, tick, logEvents, documentEvents, run: code => vm.runInContext(code, context) };
}

test("near-bottom reading is not bottom-following; fractional bottom and short logs are", () => {
  const h = harness();
  assert.equal(h.run("atBottom()"), false);
  h.log.scrollTop = 1398.5;
  assert.equal(h.run("atBottom()"), true);
  h.col.offsetHeight = 400; h.log.scrollTop = 0;
  assert.equal(h.run("atBottom()"), true);
});

test("inertial scroll bursts postpone read mutations until quiet", () => {
  const h = harness();
  for(let i = 0; i < 20; i++){
    h.run("scheduleReadProgress()"); h.tick(50);
    assert.deepEqual(h.calls, []);
  }
  h.tick(160);
  assert.deepEqual(h.calls, ["advance", "render"]);
});

test("held touch and live animations postpone read mutations", () => {
  const h = harness();
  h.run("readTouching = true; scheduleReadProgress()"); h.tick(500);
  assert.deepEqual(h.calls, []);
  h.run("readTouching = false; liveBusy = true; scheduleReadProgress()"); h.tick(500);
  assert.deepEqual(h.calls, []);
  h.run("liveBusy = false; flushReadProgress()");
  assert.deepEqual(h.calls, ["advance", "render"]);
});

test("watching the bottom stays read with a resting finger and programmatic scroll timers", () => {
  const h = harness();
  h.log.scrollTop = 1400;
  h.run('model.upsertUser(1, { seq: 1 }, "run"); model.appendBlock(1, 1, { text: "answer" })');
  h.run("readTouching = true");
  assert.equal(h.run("markReadActual(1)"), true);
  h.run('model.appendBlock(1, 1, { text: "next" })');
  h.run("readTouching = false; scheduleReadProgress()");
  assert.equal(h.run("markReadActual(1)"), true);
  assert.equal(h.run("readThrough[1]"), pos(1, 1));
});

test("touch release on another surface clears a multi-touch gesture", () => {
  const h = harness();
  h.run('initReadTouch(document.getElementById("log"))');
  h.logEvents.touchstart();
  h.documentEvents.touchend({ touches: [{}] });
  assert.equal(h.run("readTouching"), true);
  h.documentEvents.touchend({ touches: [] });
  assert.equal(h.run("readTouching"), false);
  h.tick(200);
  assert.deepEqual(h.calls, ["advance", "render"]);
});

test("continuous live commits keep watermarks current and allow window maintenance", () => {
  const h = harness();
  h.log.scrollTop = 1400;
  h.run(`
    markRead = markReadActual; commitLive = commitLiveActual;
    readThrough[1] = 0; stick = true;
    model.upsertUser(1, { seq: 1 }, "run");
    rerender = () => {
      const sc = document.getElementById("log"), col = document.getElementById("logcol");
      col.offsetHeight += 40;
      sc.scrollHeight = col.offsetHeight;
      sc.scrollTop = col.offsetHeight - sc.clientHeight;
      stick = atBottom();
      scheduleReadProgress();
      return { motionMS: 180 };
    };
  `);
  for(let i = 0; i < 120; i++){
    h.run('model.appendBlock(1, 1, { text: "next" }); host.onAffected(new Set([1]))');
    h.tick(50);
    assert.equal(h.run("readThrough[1]"), pos(1, i));
  }
  assert.ok(h.calls.includes("cap"));
  assert.equal(h.run("rawUnreadCount(1)"), 0);
});

test("explicit lifecycle reads bypass a held touch and leave no deferred work after reset", () => {
  const h = harness();
  h.run('model.upsertUser(1, { seq: 1 }, "run"); model.appendBlock(1, 1, { text: "answer" })');
  h.run("readTouching = true; scheduleReadProgress()");
  assert.equal(h.run("markReadActual(1, true)"), true);
  assert.equal(h.run("readThrough[1]"), pos(1, 0));
  h.run("resetReadScroll()"); h.tick(500);
  assert.deepEqual(h.calls, []);
  assert.equal(h.run("readTouching || readScrollTimer || readScrollReady"), 0);
});

test("a pending read cannot mutate a different session or a hidden page", () => {
  const h = harness();
  h.run("scheduleReadProgress(); active = 2"); h.tick(200);
  assert.deepEqual(h.calls, []);
  h.run('scheduleReadProgress(); document.visibilityState = "hidden"'); h.tick(200);
  assert.deepEqual(h.calls, []);
});

test("read-through preserves distance when offscreen content height changes", () => {
  const h = harness();
  h.run('rerenderStructural = () => { document.getElementById("log").scrollHeight -= 37; }');
  h.run("scheduleReadProgress()"); h.tick(200);
  assert.equal(h.log.scrollTop, 1313);
  assert.equal(h.log.scrollHeight - h.log.scrollTop - h.log.clientHeight, 50);
  assert.deepEqual(h.calls, ["advance"]);
});

test("fully visible unread content can settle without a scroll event or stick intent", () => {
  const h = harness();
  h.col.offsetHeight = 400; h.log.scrollTop = 0;
  h.run("stick = false; readOnScroll = false; scheduleReadProgress()"); h.tick(200);
  assert.deepEqual(h.calls, ["read", "cap", "animate"]);
});

test("large histories are capped after reaching bottom, not in the approach zone", () => {
  const h = harness();
  h.run('capWindow = () => { calls.push("cap"); return 10; }');
  h.run("scheduleReadProgress()"); h.tick(200);
  assert.deepEqual(h.calls, ["advance", "render"]);
  h.calls.length = 0; h.log.scrollTop = 1400;
  h.run("scheduleReadProgress()"); h.tick(200);
  assert.deepEqual(h.calls, ["read", "cap", "render"]);
});
