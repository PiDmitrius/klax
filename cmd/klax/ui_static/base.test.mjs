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
  const timers = new Map(), calls = [], uploads = [];
  let id = 0, now = 0;
  class XMLHttpRequest {
    constructor(){ this.upload = {}; this.headers = {}; }
    open(method, url){ this.method = method; this.url = url; }
    setRequestHeader(name, value){ this.headers[name] = value; }
    getAllResponseHeaders(){ return "content-type: application/json\r\nx-result: accepted\r\n"; }
    send(body){ this.body = body; uploads.push(this); }
    abort(){ this.aborted = true; this.onabort(); }
    respond(status, text = ""){
      this.status = status; this.statusText = "";
      this.response = new TextEncoder().encode(text).buffer;
      this.onload();
    }
  }
  const context = {
    AbortController, DOMException, Response, Headers, FormData, XMLHttpRequest,
    location: { pathname: "/mount/" }, localStorage: { getItem: () => "token" },
    document: { getElementById: () => ({ textContent: JSON.stringify({ request_ms: 10000, poll_ms: 30000, retry_min_ms: 625, retry_max_ms: 5000 }) }) },
    setTimeout: (fn, ms) => { timers.set(++id, { fn, ms, at: now + ms }); return id; },
    clearTimeout: id => timers.delete(id),
    fetch: (url, opts) => new Promise((resolve, reject) => {
      opts.signal.addEventListener("abort", () => reject(opts.signal.reason), { once: true });
      calls.push({ url, opts, resolve });
    }),
  };
  const source = readFileSync(new URL("./base.js", import.meta.url), "utf8").replace(/export /g, "");
  runInNewContext(source + "\nthis.api = api; this.retryDelay = retryDelay;", context);
  function tick(ms){
    const end = now + ms;
    while(true){
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if(!due) break;
      now = due[1].at; timers.delete(due[0]); due[1].fn();
    }
    now = end;
  }
  return { context, timers, calls, uploads, tick };
}

test("multipart uploads can take longer than ten seconds while bytes keep moving", async () => {
  const h = requests(), body = new FormData();
  body.append("files", new Blob(["attachment"]), "file.txt");
  const flight = h.context.api("/api/send", { method: "POST", body });
  flight.catch(() => {});
  const xhr = h.uploads[0] || {
    url: h.calls[0].url, headers: h.calls[0].opts.headers, body: h.calls[0].opts.body,
    upload: { onprogress() {} },
    get aborted(){ return h.calls[0].opts.signal.aborted; },
    respond(status){ h.calls[0].resolve(new Response(null, { status })); },
  };
  assert.equal(xhr.url, "/mount/api/send");
  assert.equal(xhr.headers.Authorization, "Bearer token");
  assert.equal(xhr.body, body);
  for(const loaded of [1, 2, 3]){
    h.tick(8000);
    xhr.upload.onprogress({ loaded });
    assert.notEqual(xhr.aborted, true);
  }
  xhr.respond(204);
  assert.equal((await flight).status, 204);
  assert.equal(h.timers.size, 0);
  assert.equal(h.calls.length, 0);
});

for(const phase of ["upload", "response wait", "response body"]){
  test(`multipart requests time out after ten seconds without progress during ${phase}`, async () => {
    const h = requests();
    const flight = h.context.api("/api/send", { method: "POST", body: new FormData() });
    const rejection = assert.rejects(flight, { name: "TimeoutError" });
    const xhr = h.uploads[0];
    h.tick(8000);
    if(phase === "upload") xhr.upload.onprogress({ loaded: 1 });
    if(phase === "response wait") xhr.upload.onload();
    if(phase === "response body") xhr.onprogress({ loaded: 1 });
    h.tick(9999);
    assert.notEqual(xhr.aborted, true);
    h.tick(1);
    await rejection;
    assert.equal(xhr.aborted, true);
    assert.equal(h.timers.size, 0);
  });
}

test("multipart cancellation keeps its reason and other requests remain independent", async () => {
  const h = requests(), controller = new AbortController();
  const upload = h.context.api("/api/send", { method: "POST", body: new FormData(), signal: controller.signal });
  const rejected = assert.rejects(upload, reason => reason === "cancelled");
  const ordinary = h.context.api("/api/state");
  controller.abort("cancelled");
  await rejected;
  assert.equal(h.uploads[0].aborted, true);
  assert.equal(h.calls[0].opts.signal.aborted, false);
  h.calls[0].resolve(Response.json({ at: "epoch.8" }));
  assert.deepEqual(await (await ordinary).json(), { at: "epoch.8" });
  assert.equal(h.timers.size, 0);
});

test("multipart replies retain API status, headers and body", async () => {
  const h = requests();
  const flight = h.context.api("/api/send", { method: "POST", body: new FormData() });
  h.uploads[0].respond(503, '{"error":{"message":"retry"}}');
  const response = await flight;
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("x-result"), "accepted");
  assert.equal((await response.json()).error.message, "retry");
  assert.equal(h.timers.size, 0);
});

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
