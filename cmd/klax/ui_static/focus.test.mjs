import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { TurnModel } from "./model.js";

function harness(coarse){
  const calls = [];
  const input = {
    focus(options){ calls.push(["focus", options.preventScroll]); document.activeElement = input; },
    blur(){ calls.push(["blur"]); document.activeElement = null; },
  };
  const document = { visibilityState: "visible", activeElement: null, getElementById: () => input };
  const source = readFileSync(new URL("./app.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").split('\napplyTheme((() =>')[0];
  const context = vm.createContext({ TurnModel, document, hasCoarsePointer: () => coarse });
  vm.runInContext(source, context);
  return { calls, input, document, select: () => context.focusComposer(), show: () => context.resetMobileComposerFocus() };
}

test("mobile initial selection clears pre-existing composer focus without opening the keyboard", () => {
  const h = harness(true);
  h.document.activeElement = h.input;
  h.select();
  assert.deepEqual(h.calls, [["blur"]]);
  assert.equal(h.document.activeElement, null);
  h.select();
  assert.deepEqual(h.calls, [["blur"]]);
});

test("mobile page restoration clears restored focus but leaves other controls alone", () => {
  const h = harness(true);
  h.document.activeElement = h.input;
  h.show();
  assert.deepEqual(h.calls, [["blur"]]);
  const other = {};
  h.document.activeElement = other;
  h.show();
  assert.equal(h.document.activeElement, other);
  assert.equal(h.calls.length, 1);
});

test("desktop selection focuses without scrolling; page restoration does not steal focus", () => {
  const h = harness(false);
  h.select();
  assert.deepEqual(h.calls, [["focus", true]]);
  const other = {};
  h.document.activeElement = other;
  h.show();
  assert.equal(h.document.activeElement, other);
});

test("hidden pages do not change focus", () => {
  for(const coarse of [true, false]){
    const h = harness(coarse);
    h.document.visibilityState = "hidden";
    h.document.activeElement = h.input;
    h.select();
    assert.deepEqual(h.calls, []);
  }
});
