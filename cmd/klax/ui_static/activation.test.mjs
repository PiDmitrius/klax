import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

function harness(){
  const events = {}, calls = [], timers = new Map();
  let next = 0;
  const source = readFileSync(new URL("./base.js", import.meta.url), "utf8").replace(/export /g, "");
  const context = vm.createContext({
    setTimeout(fn){ const id = ++next; timers.set(id, fn); return id; },
    clearTimeout(id){ timers.delete(id); },
  });
  vm.runInContext(source, context);
  context.bindButtonActivation({ addEventListener: (name, fn) => { events[name] = fn; } }, touch => calls.push(touch));
  return { events, calls, expire(){ for(const fn of timers.values()) fn(); timers.clear(); } };
}

test("touch acts before default blur and does not act twice on the following click", () => {
  const h = harness();
  let prevented = false;
  h.events.pointerdown({ pointerType: "touch", preventDefault(){ prevented = true; } });
  assert.equal(prevented, true);
  assert.deepEqual(h.calls, [true]);
  h.events.click();
  assert.deepEqual(h.calls, [true]);
  h.events.click();
  assert.deepEqual(h.calls, [true, false]);
});

test("mouse and keyboard retain normal click activation", () => {
  const h = harness();
  h.events.pointerdown({ pointerType: "mouse", preventDefault(){ assert.fail("mouse default must be preserved"); } });
  assert.deepEqual(h.calls, []);
  h.events.click();
  assert.deepEqual(h.calls, [false]);
});

test("missing synthetic click does not suppress subsequent activations indefinitely", () => {
  const h = harness();
  h.events.pointerdown({ pointerType: "touch", preventDefault(){} });
  h.expire();
  h.events.click();
  assert.deepEqual(h.calls, [true, false]);
});
