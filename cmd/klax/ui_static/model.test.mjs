import { test } from "node:test";
import assert from "node:assert/strict";
import { TurnModel, ordLess } from "./model.js";

const head = (seq, state = "done") => ({ turn_seq: seq, role: "user", state });
const window = groups => ({ from: groups.length ? groups[0].ord : null, groups });

test("ord sorts by record, then turn; a turn without a record sorts last; null is the history start", () => {
  assert.ok(ordLess("1@3", "2@8"));
  assert.ok(ordLess("2@8", "3@8"));
  assert.ok(ordLess("3@8", "4"));
  assert.ok(!ordLess("4", "1@9"));
  assert.ok(ordLess("0@-1", "1@3"));
  assert.ok(ordLess("-9@2", "5@2"));
  assert.ok(ordLess(null, "0@-1"));
  assert.ok(!ordLess("0@-1", null));
});

test("turns flattens groups into user rows with blocks followed by their standalone rows, keyed by group and index", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([
    { key: "t:1:0", ord: "0@-1", head: null, blocks: null, rows: [{ role: "system", text: "intro" }] },
    { key: "t:1:5", ord: "5@2", head: head(5), blocks: [{ id: "a" }], rows: [{ role: "tool", text: "note" }] },
  ]));
  assert.deepEqual(m.turns(1).map(t => t.role + ":" + (t.turn_seq || t.text)), ["system:intro", "user:5", "tool:note"]);
  assert.deepEqual(m.turns(1)[1].blocks, [{ id: "a" }]);
  assert.deepEqual([m.turns(1)[0].key, m.turns(1)[2].key], ["t:1:0:0", "t:1:5:0"]);
});

test("a group delta keeps the prefix, appends the suffix, cuts to n and merges the header", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: head(5, "run"), blocks: [{ id: "a" }, { id: "b" }] }]));
  assert.ok(m.applyGroup(1, { key: "t:1:5", ord: "5@2", head: { state: "done" }, blocks: { set: { 1: { id: "b2" } }, start: 2, append: [{ id: "c" }] } }));
  assert.deepEqual(m.turns(1)[0].blocks.map(b => b.id), ["a", "b2", "c"]);
  assert.equal(m.turns(1)[0].state, "done");
  assert.ok(m.applyGroup(1, { key: "t:1:5", ord: "5@2", blocks: { length: 1 } }));
  assert.deepEqual(m.turns(1)[0].blocks.map(b => b.id), ["a"]);
  assert.equal(m.turns(1)[0].state, "done");
});

test("new groups insert in ord order; older ones are left to paging; a lost delta asks for a reload", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: head(5) }]));
  assert.ok(m.applyGroup(1, { key: "t:1:7", ord: "7", create: { head: head(7, "enq"), blocks: [] } }));
  assert.ok(m.applyGroup(1, { key: "t:1:6", ord: "6@3", create: { head: head(6), blocks: [] } }));
  assert.ok(m.applyGroup(1, { key: "t:1:1", ord: "1@0", create: { head: head(1), blocks: [] } }));
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [5, 6, 7]);
  assert.equal(m.applyGroup(1, { key: "t:1:9", ord: "9@4", blocks: { start: 1, append: [{}] } }), false);
});

test("a held group moving below the range asks for a reload; removal drops a key", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: head(5) }, { key: "t:1:22", ord: "22", head: head(22, "enq") }]));
  assert.equal(m.applyGroup(1, { key: "t:1:22", ord: "22@1" }), false);
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [5, 22]);
  m.applyRemoved(1, "t:1:5");
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [22]);
});

test("a page replaces its range and extends the start; eviction drops whole groups", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@5", head: head(5) }, { key: "t:1:6", ord: "6@6", head: head(6) }]));
  m.loadPage(1, { from: "1@1", to: "5@5", groups: [
    { key: "t:1:1", ord: "1@1", head: head(1), rows: [{ role: "system", text: "s" }] },
    { key: "t:1:3", ord: "3@3", head: head(3) },
  ] });
  assert.deepEqual(m.rangeStart(1), "1@1");
  assert.deepEqual(m.turns(1).map(t => t.turn_seq || t.text), [1, "s", 3, 5, 6]);
  assert.equal(m.evictTop(1, 1), 0);
  assert.equal(m.evictTop(1, 3), 3);
  assert.deepEqual(m.rangeStart(1), "5@5");
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [5, 6]);
});

test("a page drops a held copy of a key it carries; a window from the history start keeps moved groups", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@5", head: head(5) }, { key: "t:1:22", ord: "22", head: head(22, "enq") }]));
  m.loadPage(1, { from: "1@1", to: "5@5", groups: [{ key: "t:1:22", ord: "22@3", head: head(22) }] });
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [22, 5]);
  m.loadWindow(1, { from: null, groups: [{ key: "t:1:7", ord: "7", head: head(7, "err") }] });
  assert.ok(m.applyGroup(1, { key: "t:1:7", ord: "7@4" }));
  assert.ok(m.applyGroup(1, { key: "t:1:8", ord: "8@4", create: { head: head(8), blocks: [] } }));
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [7, 8]);
});

test("a context-only delta updates the head's usage and keeps its text", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: { ...head(5, "run"), text: "long prompt" }, blocks: [{ id: "a" }] }]));
  assert.ok(m.applyGroup(1, { key: "t:1:5", ord: "5@2", head: { ctx_used: 900 } }));
  const t = m.turns(1)[0];
  assert.equal(t.text, "long prompt");
  assert.equal(t.ctx_used, 900);
  assert.deepEqual(t.blocks.map(b => b.id), ["a"]);
});

test("a repeated group delta changes nothing", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: head(5, "run"), blocks: [{ id: "a" }] }]));
  const d = { key: "t:1:5", ord: "5@2", head: { state: "done" }, blocks: { start: 1, append: [{ id: "b" }] } };
  m.applyGroup(1, d);
  m.applyGroup(1, d);
  assert.deepEqual(m.turns(1)[0].blocks.map(b => b.id), ["a", "b"]);
});

test("a delta for an array shorter than it expects asks for a reload", () => {
  const m = new TurnModel();
  m.loadWindow(1, window([{ key: "t:1:5", ord: "5@2", head: head(5, "run"), blocks: [{ id: "a" }] }]));
  assert.equal(m.applyGroup(1, { key: "t:1:5", ord: "5@2", blocks: { start: 3, append: [{ id: "d" }] } }), false);
  assert.equal(m.applyGroup(1, { key: "t:1:5", ord: "5@2", blocks: { set: { 2: { id: "c" } } } }), false);
  assert.deepEqual(m.turns(1)[0].blocks.map(b => b.id), ["a"]);
});

test("eviction keeps the range starting at a transcript position", () => {
  const q = new TurnModel();
  q.loadWindow(1, window([
    { key: "t:1:5", ord: "5@5", head: head(5) },
    { key: "t:1:6", ord: "6@9", head: head(6, "err") },
    { key: "t:1:7", ord: "7@9", head: head(7) },
  ]));
  assert.equal(q.evictTop(1, 1), 0);
  assert.deepEqual(q.rangeStart(1), "5@5");

  const m = new TurnModel();
  m.loadWindow(1, window([
    { key: "t:1:5", ord: "5@5", head: head(5) },
    { key: "t:1:6", ord: "6", head: head(6, "enq") },
    { key: "t:1:7", ord: "7", head: head(7, "enq") },
  ]));
  assert.equal(m.evictTop(1, 2), 0);
  assert.deepEqual(m.rangeStart(1), "5@5");
  assert.ok(m.applyGroup(1, { key: "t:1:6", ord: "6@9", head: { state: "run" } }));
  assert.deepEqual(m.turns(1).map(t => t.turn_seq), [5, 6, 7]);
});
