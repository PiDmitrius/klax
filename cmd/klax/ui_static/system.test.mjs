import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

function harness(){
  const rows = [];
  const body = { append: (...values) => rows.push(...values), appendChild() {} };
  const modal = { classList: { contains: () => true } };
  const context = vm.createContext({
    rows, isReadOnly: () => true, clearTimeout() {}, setTimeout: () => 0,
    document: { getElementById: id => id === "sysbody" ? body : modal, createElement: () => ({}) },
  });
  const source = readFileSync(new URL("./system.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
  vm.runInContext(source + '\nrow = (label, value) => ({ label, value });', context);
  return { rows, run: code => vm.runInContext(code, context) };
}

test("system display shows RSS and cumulative CPU time", () => {
  const h = harness();
  h.run(`render({ version: "0.9.0", started_at: "2020-01-01T00:00:00Z", uptime_sec: 61,
    rss_bytes: 20.25 * 1024 * 1024, cpu_time_sec: 61.25, platform: "linux/amd64", update: {} })`);
  const values = Object.fromEntries(h.rows.map(r => [r.label, r.value]));
  assert.equal(values.RAM, "20.3 МиБ");
  assert.equal(values["CPU Time"], "1 мин 1.3 с");
  assert.equal(values["Работает"], "1 мин 1 с");
  assert.equal(Object.hasOwn(values, "Процесс"), false);
});

test("unavailable resource metrics remain unknown", () => {
  const h = harness();
  h.run('render({ rss_bytes: null, cpu_time_sec: null, update: {} })');
  const values = Object.fromEntries(h.rows.map(r => [r.label, r.value]));
  assert.equal(values.RAM, "—");
  assert.equal(values["CPU Time"], "—");
});

test("durations retain CPU fractions and carry rounded seconds", () => {
  const h = harness();
  for(const [sec, text] of [[0, "0 с"], [0.14, "0.1 с"], [59.96, "1 мин 0 с"], [3661.24, "1 ч 1 мин 1.2 с"], [86400, "1 д 0 с"]]){
    assert.equal(h.run(`elapsed(${sec})`), text);
  }
});
