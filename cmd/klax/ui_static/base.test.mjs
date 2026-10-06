import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { setHome, tildePath, apiError } from "./base.js";

test("paths under the server home are shown with ~, others unchanged", () => {
  setHome("/home/u");
  assert.equal(tildePath("/home/u"), "~");
  assert.equal(tildePath("/home/u/work/x"), "~/work/x");
  assert.equal(tildePath("/home/user2/work"), "/home/user2/work");
  assert.equal(tildePath("/srv"), "/srv");
  assert.equal(tildePath(""), "");
  setHome("");
  assert.equal(tildePath("/home/u/work"), "/home/u/work");
});

test("an API error shows its message, or the fallback for an unreadable body", async () => {
  const json = body => ({ json: async () => body });
  assert.equal(await apiError(json({ error: { code: "invalid-settings", message: "Каталог недоступен" } }), "fallback"), "Каталог недоступен");
  assert.equal(await apiError(json({}), "fallback"), "fallback");
  assert.equal(await apiError({ json: async () => { throw new Error("not json"); } }, "fallback"), "fallback");
});

function requests(){
  const timers = new Map(), calls = [];
  let id = 0;
  const context = {
    AbortController, DOMException, Response,
    location: { pathname: "/mount/" }, localStorage: { getItem: () => "token" },
    document: { getElementById: () => ({ textContent: JSON.stringify({ request_ms: 10000, poll_ms: 30000, retry_min_ms: 625, retry_max_ms: 5000 }) }) },
    setTimeout: (fn, ms) => { timers.set(++id, { fn, ms }); return id; },
    clearTimeout: id => timers.delete(id),
    fetch: (url, opts) => new Promise((resolve, reject) => {
      opts.signal.addEventListener("abort", () => reject(opts.signal.reason), { once: true });
      calls.push({ url, opts, resolve });
    }),
  };
  const source = readFileSync(new URL("./base.js", import.meta.url), "utf8").replace(/export /g, "");
  runInNewContext(source + "\nthis.api = api; this.retryDelay = retryDelay;", context);
  return { context, timers, calls };
}

test("the common API deadline aborts a request whose headers never arrive", async () => {
  const h = requests(), flight = h.context.api("/api/settings");
  const rejection = assert.rejects(flight, { name: "TimeoutError" });
  assert.equal(h.calls[0].url, "/mount/api/settings");
  assert.equal(h.calls[0].opts.headers.Authorization, "Bearer token");
  assert.equal(h.timers.values().next().value.ms, 10000);
  h.timers.values().next().value.fn();
  await rejection;
  assert.equal(h.calls[0].opts.signal.aborted, true);
  assert.equal(h.timers.size, 0);
});

test("receiving headers does not clear the API deadline while the body is stalled", async () => {
  const h = requests(), flight = h.context.api("/api/state");
  const rejection = assert.rejects(flight, { name: "TimeoutError" });
  const stream = new ReadableStream({
    start(controller){ h.calls[0].opts.signal.addEventListener("abort", () => controller.error(h.calls[0].opts.signal.reason)); },
  });
  h.calls[0].resolve(new Response(stream));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.timers.size, 1);
  h.timers.values().next().value.fn();
  await rejection;
  assert.equal(h.timers.size, 0);
});

test("a held poll keeps its longer deadline while an ordinary request times out", async () => {
  const h = requests();
  const poll = h.context.api("/api/changes", { method: "POST" }, true);
  const ordinary = h.context.api("/api/settings");
  const rejectedOrdinary = assert.rejects(ordinary, { name: "TimeoutError" });
  const [pollTimer, ordinaryTimer] = h.timers.values();
  assert.equal(pollTimer.ms, 30000);
  assert.equal(ordinaryTimer.ms, 10000);
  ordinaryTimer.fn();
  await rejectedOrdinary;
  assert.equal(h.calls[0].opts.signal.aborted, false);
  const rejectedPoll = assert.rejects(poll, { name: "TimeoutError" });
  const stream = new ReadableStream({
    start(controller){ h.calls[0].opts.signal.addEventListener("abort", () => controller.error(h.calls[0].opts.signal.reason)); },
  });
  h.calls[0].resolve(new Response(stream));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.timers.size, 1);
  pollTimer.fn();
  await rejectedPoll;
  assert.equal(h.timers.size, 0);
});

test("caller cancellation keeps its reason and does not abort another request", async () => {
  const h = requests(), controller = new AbortController();
  const cancelled = h.context.api("/api/send", { signal: controller.signal });
  const good = h.context.api("/api/read");
  const rejection = assert.rejects(cancelled, reason => reason === "cancelled");
  controller.abort("cancelled");
  await rejection;
  assert.equal(h.calls[1].opts.signal.aborted, false);
  h.calls[1].resolve(new Response(null, { status: 204 }));
  assert.equal((await good).status, 204);
  assert.equal(h.timers.size, 0);
});

test("the complete JSON response clears its deadline and retry pauses stop at five seconds", async () => {
  const h = requests(), flight = h.context.api("/api/state");
  h.calls[0].resolve(Response.json({ at: "epoch.7" }));
  assert.deepEqual(await (await flight).json(), { at: "epoch.7" });
  assert.equal(h.timers.size, 0);
  assert.deepEqual([0, 1, 2, 3, 4, 1000000].map(h.context.retryDelay), [625, 1250, 2500, 5000, 5000, 5000]);
});
