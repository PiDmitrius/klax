package main

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// serveModule serves an embedded ES module with the right MIME and rejects traversal /
// missing files; handleSPA dispatches *.js / *.css to it without needing auth or a daemon.
func TestServeModule(t *testing.T) {
	s := &uiServer{}

	rec := httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/model.js", nil), "/model.js")
	if rec.Code != 200 {
		t.Fatalf("model.js: code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("model.js content-type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "TurnModel") {
		t.Fatal("model.js body missing TurnModel")
	}

	rec = httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/nope.js", nil), "/nope.js")
	if rec.Code != 404 {
		t.Fatalf("missing module should 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/x", nil), "/sub/x.js")
	if rec.Code != 404 {
		t.Fatalf("path with a slash (traversal) should 404, got %d", rec.Code)
	}

	// handleSPA dispatches a .js path to serveModule (no daemon/auth needed for assets).
	rec = httptest.NewRecorder()
	s.handleSPA(rec, httptest.NewRequest("GET", "/render.js", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "renderModel") {
		t.Fatalf("handleSPA /render.js: code %d", rec.Code)
	}
}

func TestTimelineDoesNotStoreSessionContextHintsOnTurns(t *testing.T) {
	files := []string{
		"ui_static/app.js",
		"ui_static/model.js",
		"ui_static/render.js",
	}
	for _, name := range files {
		body, err := moduleFS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		src := string(body)
		for _, bad := range []string{"ctx_hint", "setContextHint", "setSessionContextHint"} {
			if strings.Contains(src, bad) {
				t.Fatalf("%s still contains stale context hint path %q", name, bad)
			}
		}
	}
}

func TestRenderModelContextFallbackAndStableGroupPositions(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	dir := t.TempDir()
	for _, name := range []string{"base.js", "markdown.js", "render.js"} {
		body, err := moduleFS.ReadFile("ui_static/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	script := `
import { renderModel, pos } from "./render.js";

function assert(cond, msg){
  if(!cond) throw new Error(msg);
}
const base = { seq: 2, role: "user", text: "u", time: "2026-01-01T00:00:00Z", blocks: [] };
// A running turn carries the PREVIOUS turn's own measured context (one source, not a snapshot).
const prev = (used, window) => ({ seq: 1, role: "user", text: "p", time: "2026-01-01T00:00:00Z", blocks: [], state: "done", ctx_used: used, ctx_window: window });
function runCtx(turn, prevUsed, prevWindow){
  const items = renderModel([prev(prevUsed, prevWindow), turn]);
  return items[items.length - 1].ctxLine;
}
assert(runCtx({ ...base, state: "run" }, 50000, 200000) === "📊 Контекст: 25% (50k/200k)", "running turn must carry the previous turn's context");
assert(runCtx({ ...base, state: "run", ctx_used: 120000, ctx_window: 200000 }, 50000, 200000) === "📊 Контекст: 60% (120k/200k)", "turn-local context must win");
assert(runCtx({ ...base, state: "done" }, 50000, 200000) === "", "done turn without its own tokens must not carry a fallback");
assert(runCtx({ ...base, state: "enq" }, 50000, 200000) === "", "queued turn must not show context fallback");
assert(runCtx({ ...base, state: "run" }, 50000, 0) === "📊 Контекст: 50k", "used-only fallback must render count");
assert(renderModel([{ ...base, state: "run" }])[0].ctxLine === "", "a lone running turn has nothing to carry");

const tools = [
  { id: "a", role: "tool", text: "one" },
  { id: "b", role: "tool", text: "two" },
  { id: "c", role: "tool", text: "three" },
];
const split = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 1))[0].groups.filter(g => !g.divider);
assert(split.length === 2, "unread divider must split a tool group");
assert(split[0].startPos === pos(2, 0), "leading split group startPos");
assert(split[1].startPos === pos(2, 2), "trailing split group startPos");

const merged = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 3))[0].groups.filter(g => !g.divider);
assert(merged.length === 1, "read groups must merge");
assert(merged[0].startPos === pos(2, 0), "merged group must inherit leading startPos");

const held = new Map([[2, new Set([pos(2, 2)])]]);
const heldGroups = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 3), held)[0].groups;
assert(heldGroups.length === 2, "held split must keep two groups after divider removal");
assert(!heldGroups.some(g => g.divider), "held split must not keep the unread divider");
assert(heldGroups[0].startPos === pos(2, 0) && heldGroups[1].startPos === pos(2, 2), "held split positions");
const joinedGroups = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 3), held, true)[0].groups;
assert(joinedGroups[0].joinNext === true && joinedGroups[1].joinPrev === true, "held split join flags");

const mixed = [
  { id: "m1", role: "assistant", text: "message" },
  { id: "m2", role: "tool", text: "tool" },
];
const mixedJoined = renderModel([{ ...base, state: "done", blocks: mixed }], pos(2, 2), new Map([[2, new Set([pos(2, 1)])]]), true)[0].groups;
assert(!mixedJoined.some(g => g.joinPrev || g.joinNext), "mixed-role held split must not join");

const oldTool = renderModel([{ ...base, state: "done", blocks: [{ id: "old", role: "tool", text: "old label" }] }], undefined)[0].groups[0];
const newTool = renderModel([{ ...base, state: "done", blocks: [{ id: "new", role: "tool", text: "new label" }] }], undefined)[0].groups[0];
assert(oldTool.startPos === newTool.startPos, "tool text/id changes must keep position key stable");

const standaloneTool = renderModel([{ role: "tool", text: "🗜 Compaction: Summary" }], undefined)[0];
assert(standaloneTool.kind === "bubble" && standaloneTool.cls === "tool" && standaloneTool.md === false, "standalone tool rows must render as tool bubbles");
`
	scriptPath := filepath.Join(dir, "render_model_test.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command("node", scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node renderModel test failed: %v\n%s", err, out)
	}
}

// The durable send-outbox is the client half of the "a submitted message is never lost" guarantee:
// recoverOutbox must keep a still-unconfirmed message for a live session, re-home one whose target
// session was closed (under a fresh nonce, dropping the undeliverable original), and discard an
// unrecoverable attachment-only entry — all without losing any written text.
func TestSendOutboxRecovery(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	dir := t.TempDir()
	for _, name := range []string{"base.js", "compose.js"} {
		body, err := moduleFS.ReadFile("ui_static/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	script := `
// Minimal localStorage mock with length/key() so the per-nonce outbox can scan its own keys.
const store = new Map();
globalThis.localStorage = {
  getItem: k => store.has(k) ? store.get(k) : null,
  setItem: (k, v) => { store.set(k, String(v)); },
  removeItem: k => { store.delete(k); },
  key: i => { const ks = Array.from(store.keys()); return i < ks.length ? ks[i] : null; },
  get length(){ return store.size; },
};
function assert(c, m){ if(!c) throw new Error(m); }
// The outbox namespaces keys by a djb2 tag of the auth token — replicate it to seed entries.
localStorage.setItem("klax_ui_token", "tok");
function idTag(t){ let h1 = 5381, h2 = 52711; for(let i = 0; i < t.length; i++){ const c = t.charCodeAt(i); h1 = ((h1 << 5) + h1 + c) >>> 0; h2 = ((h2 << 5) + h2 + (c ^ 0x9e)) >>> 0; } return h1.toString(36) + h2.toString(36); }
const K = n => "klax_ob." + idTag("tok") + "." + n;
// Session 1 is live; session 2 is closed. TWO unconfirmed messages for session 1 (must BOTH surface,
// not just the first), one orphan for the closed session 2 (re-homes to session 1), one empty (dropped).
localStorage.setItem(K("a1"),    JSON.stringify({ created: 1, text: "first",  nonce: "a1",    sent: true }));
localStorage.setItem(K("a2"),    JSON.stringify({ created: 1, text: "second", nonce: "a2",    sent: true }));
localStorage.setItem(K("orph"),  JSON.stringify({ created: 2, text: "orphan", nonce: "orph",  sent: true }));
localStorage.setItem(K("empty"), JSON.stringify({ created: 1, text: "",       nonce: "empty", sent: true }));
// A different identity's entry must be invisible to this identity's recovery (privacy namespacing).
localStorage.setItem("klax_ob.OTHER.x", JSON.stringify({ created: 1, text: "not mine", nonce: "x", sent: true }));

const { recoverOutbox } = await import("./compose.js");
let notices = 0;
const n = recoverOutbox({ isLive: c => c === 1, firstLive: () => 1, notice: () => notices++ });

const mine = () => { const p = "klax_ob." + idTag("tok") + "."; const out = []; for(let i = 0; i < localStorage.length; i++){ const k = localStorage.key(i); if(k && k.indexOf(p) === 0) out.push(JSON.parse(localStorage.getItem(k))); } return out; };
const after = mine();
// The three original per-message nonces and the empty entry are all gone (consolidated / dropped).
["a1", "a2", "orph", "empty"].forEach(x => assert(!after.some(e => e.nonce === x), "original nonce " + x + " must not remain"));
// Exactly ONE consolidated entry for session 1, holding ALL recovered texts (none stranded).
assert(after.length === 1, "expected a single consolidated entry, got " + after.length);
const c = after[0];
assert(c.created === 1 && c.text === "first\n\nsecond\n\norphan", "consolidated text must contain every recovered message: " + JSON.stringify(c.text));
assert(localStorage.getItem("klax_ob.OTHER.x") !== null, "another identity's entry must be left untouched");
assert(n === 3, "three messages recovered (count must not over-report), got " + n);
assert(notices === 1, "exactly one recovery notice");
console.log("ok");
`
	scriptPath := filepath.Join(dir, "outbox_test.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command("node", scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node outbox test failed: %v\n%s", err, out)
	}
}
