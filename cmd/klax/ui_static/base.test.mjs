import { test } from "node:test";
import assert from "node:assert/strict";
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
