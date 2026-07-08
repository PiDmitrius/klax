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
function ctx(turn, hint){
  return renderModel([turn], undefined, hint)[0].ctxLine;
}

const base = { seq: 2, role: "user", text: "u", time: "2026-01-01T00:00:00Z", blocks: [] };
assert(ctx({ ...base, state: "run" }, { used: 50000, window: 200000 }) === "📊 Контекст: 25% (50k/200k)", "running turn must show session fallback");
assert(ctx({ ...base, state: "run", ctx_used: 120000, ctx_window: 200000 }, { used: 50000, window: 200000 }) === "📊 Контекст: 60% (120k/200k)", "turn-local context must win");
assert(ctx({ ...base, state: "done" }, { used: 50000, window: 200000 }) === "", "done turn must not use session fallback");
assert(ctx({ ...base, state: "enq" }, { used: 50000, window: 200000 }) === "", "queued turn must not show context fallback");
assert(ctx({ ...base, state: "run" }, { used: 50000, window: 0 }) === "📊 Контекст: 50k", "used-only fallback must render count");

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
const heldGroups = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 3), null, held)[0].groups;
assert(heldGroups.length === 2, "held split must keep two groups after divider removal");
assert(!heldGroups.some(g => g.divider), "held split must not keep the unread divider");
assert(heldGroups[0].startPos === pos(2, 0) && heldGroups[1].startPos === pos(2, 2), "held split positions");
const joinedGroups = renderModel([{ ...base, state: "done", blocks: tools }], pos(2, 3), null, held, true)[0].groups;
assert(joinedGroups[0].joinNext === true && joinedGroups[1].joinPrev === true, "held split join flags");

const mixed = [
  { id: "m1", role: "assistant", text: "message" },
  { id: "m2", role: "tool", text: "tool" },
];
const mixedJoined = renderModel([{ ...base, state: "done", blocks: mixed }], pos(2, 2), null, new Map([[2, new Set([pos(2, 1)])]]), true)[0].groups;
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
