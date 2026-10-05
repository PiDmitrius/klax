import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { setHome, tildePath } from "./base.js";
import { fmtDate, fmtTime } from "./markdown.js";

function harness(){
  const rows = [];
  const body = { append: (...values) => rows.push(...values), appendChild() {} };
  const modal = { classList: { contains: () => true } };
  const context = vm.createContext({
    rows, tildePath, fmtDate, fmtTime, isReadOnly: () => true, clearTimeout() {}, setTimeout: () => 0,
    document: { getElementById: id => id === "sysbody" ? body : modal, createElement: () => ({}) },
  });
  const source = readFileSync(new URL("./system.js", import.meta.url), "utf8")
    .replace(/^import .*;\n/gm, "").replace(/^export /gm, "");
  vm.runInContext(source + '\nrow = (label, value) => ({ label, value });', context);
  return { rows, run: code => vm.runInContext(code, context) };
}

test("source directory stays absolute in data and is abbreviated for display", () => {
  const h = harness();
  setHome("/home/u");
  try {
    h.run('render({ update: { source_dir: "/home/u/work/klax" } })');
    assert.equal(h.rows.find(r => r.label === "Исходник").value, "~/work/klax");
    assert.equal(h.run("lastData.update.source_dir"), "/home/u/work/klax");
  } finally { setHome(""); }
});

test("system display orders elapsed time, CPU time, current and peak RSS", () => {
  const h = harness();
  h.run(`render({ version: "0.9.0", started_at: new Date(2001, 1, 3, 4, 5, 6).toISOString(), uptime_sec: 990,
    cpu_time_sec: 72, rss_bytes: 46.125 * 1024 * 1024, rss_peak_bytes: Math.round(1.1 * 1024 ** 3), platform: "linux/amd64", update: {} })`);
  const values = Object.fromEntries(h.rows.map(r => [r.label, r.value]));
  assert.equal(values["Запущен"], "2001.02.03 04:05:06");
  assert.deepEqual(h.rows.slice(2, 6).map(r => r.label), ["Работает", "Занят", "Рабочая RAM", "Максимум RAM"]);
  assert.equal(values["Работает"], "16 мин 30 с");
  assert.equal(values["Занят"], "1 мин 12 с");
  assert.equal(values["Рабочая RAM"], "46.1 МиБ");
  assert.equal(values["Максимум RAM"], "1.1 ГиБ");
  assert.equal(Object.hasOwn(values, "Процесс"), false);
});

test("unavailable resource metrics remain unknown", () => {
  const h = harness();
  h.run('render({ rss_bytes: null, rss_peak_bytes: null, cpu_time_sec: null, update: {} })');
  const values = Object.fromEntries(h.rows.map(r => [r.label, r.value]));
  assert.equal(values["Рабочая RAM"], "—");
  assert.equal(values["Максимум RAM"], "—");
  assert.equal(values["Занят"], "—");
});

test("durations retain CPU fractions and carry rounded seconds", () => {
  const h = harness();
  for(const [sec, text] of [[0, "0 с"], [0.14, "0.1 с"], [59.96, "1 мин 0 с"], [3661.24, "1 ч 1 мин 1.2 с"], [86400, "1 д 0 с"]]){
    assert.equal(h.run(`elapsed(${sec})`), text);
  }
});
