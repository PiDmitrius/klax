// Tests for the pure surface of scope.js and selection.js. Run with:
//
//     node --test cmd/klax/ui_static/
//
// No test framework and no dependencies: Node's built-in runner, the same way `go test` needs
// nothing installed. Only two globals are stubbed — `location`, which parseHash reads, and
// `history`, without which writeHash would silently fall back to assigning location.hash and the
// intended replace-without-navigation path would never be exercised. scope.js attaches its
// dismissal listeners under `typeof document !== "undefined"`, so importing it here is inert.
//
// The module keeps one piece of state (the current scope), so these tests are stateful by nature:
// each resets it, and none may run concurrently.

import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";

import {
  ROOT, parseHash, setScope, currentScope, sameScope, isRoot, inScope, filterScope,
  storageKey, hashFor, writeHash, titlePrefix, knownGroups,
} from "./scope.js";
import { neighborIn } from "./selection.js";

function at(hash){
  globalThis.location = { hash, pathname: "/klax/", search: "" };
  globalThis.history = {
    replaceState(_state, _title, url){ globalThis.history.lastURL = url; },
    lastURL: "",
  };
}

const sess = (created, ...groups) => ({ created, groups });

beforeEach(() => { setScope(ROOT); at(""); });

test("parseHash tells a session id, a group and a computed view apart", () => {
  at("#1783809783");
  assert.deepEqual(parseHash(), { scope: ROOT, created: 1783809783 });

  at("#work");
  assert.deepEqual(parseHash(), { scope: { kind: "group", name: "work" }, created: 0 });

  at("#work/1783809783");
  assert.deepEqual(parseHash(), { scope: { kind: "group", name: "work" }, created: 1783809783 });

  at("#is:unread");
  assert.deepEqual(parseHash(), { scope: { kind: "builtin", name: "is:unread" }, created: 0 });

  // An empty or absent fragment is the root, not a group with an empty name.
  at("");
  assert.deepEqual(parseHash(), { scope: ROOT, created: 0 });
  at("#");
  assert.deepEqual(parseHash(), { scope: ROOT, created: 0 });
});

test("parseHash decodes percent-encoded names and survives a malformed escape", () => {
  at("#" + encodeURIComponent("дом"));
  assert.equal(parseHash().scope.name, "дом");

  at("#" + encodeURIComponent("дом") + "/42");
  assert.deepEqual(parseHash(), { scope: { kind: "group", name: "дом" }, created: 42 });

  // A hand-mangled address must not throw, and must degrade to the RAW segment — asserting the name
  // itself, so an implementation that truncated or substituted it could not pass.
  at("#%E0%A4%A");
  assert.doesNotThrow(parseHash);
  assert.deepEqual(parseHash(), { scope: { kind: "group", name: "%E0%A4%A" }, created: 0 });
});

test("a non-numeric second segment is not a session id", () => {
  at("#work/notanid");
  assert.deepEqual(parseHash(), { scope: { kind: "group", name: "work" }, created: 0 });
});

test("hashFor round-trips through parseHash for every scope", () => {
  const cases = [
    [ROOT, 0, ""],
    [ROOT, 42, "42"],
    [{ kind: "group", name: "work" }, 0, "work"],
    [{ kind: "group", name: "work" }, 42, "work/42"],
    [{ kind: "builtin", name: "is:unread" }, 42, "is:unread/42"], // the colon stays readable
  ];
  for(const [scope, created, want] of cases){
    setScope(scope);
    assert.equal(hashFor(created), want);
    at("#" + want);
    const back = parseHash();
    assert.ok(sameScope(back.scope, scope), `${want}: scope round-trip`);
    assert.equal(back.created, created, `${want}: created round-trip`);
  }
});

test("writeHash replaces the address instead of navigating", () => {
  setScope({ kind: "group", name: "work" });
  at("#work");
  writeHash(42);
  assert.equal(globalThis.history.lastURL, "/klax/#work/42");

  // Already correct: no history write at all, so a repeated reconcile cannot spam the address bar.
  at("#work/42");
  globalThis.history.lastURL = "";
  writeHash(42);
  assert.equal(globalThis.history.lastURL, "");
});

test("membership is by group, root holds everything, a computed view holds nothing yet", () => {
  const list = [sess(1, "work"), sess(2, "work", "klax"), sess(3), sess(4, "klax")];

  setScope(ROOT);
  assert.ok(isRoot());
  assert.deepEqual(filterScope(list).map(s => s.created), [1, 2, 3, 4]);

  setScope({ kind: "group", name: "work" });
  assert.deepEqual(filterScope(list).map(s => s.created), [1, 2]);
  assert.equal(inScope(sess(9)), false);
  assert.equal(inScope(undefined), false);

  setScope({ kind: "builtin", name: "is:unread" });
  assert.deepEqual(filterScope(list), []);
});

test("each scope remembers its own tab under its own key", () => {
  setScope(ROOT);
  const root = storageKey();
  setScope({ kind: "group", name: "work" });
  const work = storageKey();
  setScope({ kind: "group", name: "klax" });
  const klax = storageKey();
  assert.equal(new Set([root, work, klax]).size, 3);
  // Root keeps the historical key, so an existing browser does not lose its last tab on upgrade.
  assert.equal(root, "klax_active");
});

test("the title carries the scope only outside root", () => {
  setScope(ROOT);
  assert.equal(titlePrefix(), "");
  setScope({ kind: "group", name: "work" });
  assert.equal(titlePrefix(), "work — ");
});

test("knownGroups derives the set from the sessions and orders it case-insensitively", () => {
  const list = [sess(1, "work", "Klax"), sess(2, "klax"), sess(3), sess(4, "work")];
  const got = knownGroups(list);

  // Derived, deduped, and a group exists only as long as a session carries it.
  assert.deepEqual(new Set(got), new Set(["work", "Klax", "klax"]));
  assert.deepEqual(knownGroups([sess(1), sess(2)]), []);
  assert.deepEqual(knownGroups(undefined), []);

  // One discriminating case proves BOTH rules at once and needs no locale assumption: a
  // case-sensitive comparator would put "Alpha" before "alpha" before "zulu" only by accident of
  // adjacency, so assert the exact sequence. Case-insensitively alpha precedes zulu; between the two
  // spellings of alpha the raw-case tiebreaker decides, and uppercase sorts first.
  assert.deepEqual(knownGroups([sess(1, "zulu", "Alpha", "alpha")]), ["Alpha", "alpha", "zulu"]);

  // No assertion on cross-alphabet collation: localeCompare uses the runtime's locale, so the order
  // of Cyrillic against Latin is not ours to pin. What must hold is that it is total and stable.
  const mixed = knownGroups([sess(1, "дом", "work", "Ёлка", "klax")]);
  assert.equal(mixed.length, 4);
  assert.deepEqual(knownGroups([sess(1, "klax", "Ёлка", "work", "дом")]), mixed);
});

test("neighborIn prefers the left tab, falls back to the right, and skips non-survivors", () => {
  const order = [sess(1), sess(2), sess(3), sess(4)];

  assert.equal(neighborIn(order, 3), 2, "left neighbour");
  assert.equal(neighborIn(order, 1), 2, "leftmost falls back to the right");

  // The neighbour to the left left in the same update: skip it rather than select into a hole.
  assert.equal(neighborIn(order, 3, [sess(1), sess(4)]), 1);
  // Nothing survives to the left, so it walks right.
  assert.equal(neighborIn(order, 2, [sess(4)]), 4);

  assert.equal(neighborIn([sess(7)], 7), 0, "sole member has no neighbour");
  assert.equal(neighborIn(order, 99), 0, "unknown session has no neighbour");
  assert.equal(neighborIn(undefined, 1), 0);
  assert.equal(neighborIn(order, 3, []), 0, "no survivors at all");

  // Reading the rule must not disturb the list it was given.
  assert.deepEqual(order.map(s => s.created), [1, 2, 3, 4]);
});
