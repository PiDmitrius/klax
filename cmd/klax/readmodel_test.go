package main

import (
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/sealref"
)

func newReadModelDaemon(t *testing.T) (*daemon, int64) {
	t.Helper()
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = newStoreWithChat("user:alice", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	s, err := sealref.New()
	if err != nil {
		t.Fatal(err)
	}
	d.sealer = s
	created := d.store.SessionsFor("user:alice")[0].Created
	d.getRunner("user:alice", created) // bind the runner-owned durable store
	return d, created
}

func testRM(d *daemon, created int64, items []history.Item, busy, latest bool) []uiTurn {
	q, _ := d.sessionStore("user:alice", created).InboundLog()
	return d.buildReadModel("user:alice", created, groupTurns(items), q, busy, 0, latest, 1_000_000)
}

// A turn still queued (enq, never run) is surfaced on the latest page as state "enq" with
// its durable seq + text, and is NOT appended on an older (paginated) page.
func TestReadModelQueuedSurfaced(t *testing.T) {
	d, created := newReadModelDaemon(t)
	sr := d.getRunner("user:alice", created)
	if _, _, _, _, err := sr.store.Enqueue("ui:alice", "", "n", "hello", nil); err != nil {
		t.Fatal(err)
	}
	turns := testRM(d, created, nil, false, true)
	if len(turns) != 1 || turns[0].State != "enq" || turns[0].Seq < 1 {
		t.Fatalf("queued turn not surfaced as enq: %+v", turns)
	}
	if !strings.HasPrefix(turns[0].Text, "hello") {
		t.Fatalf("durable text not used: %q", turns[0].Text)
	}
	if older := testRM(d, created, nil, false, false); len(older) != 0 {
		t.Fatalf("older page must not append pending: %+v", older)
	}
}

// A run turn in the transcript renders "run" only while the session is busy and it is the
// newest run; an idle session (a missed MarkDone) resolves it to "done". While running, the
// most-recent (in-progress) block is HELD back — represented by the working dots — so it never
// shows as a settled bubble under the dots; the final block is revealed only at done.
func TestReadModelRunningVsStale(t *testing.T) {
	d, created := newReadModelDaemon(t)
	sr := d.getRunner("user:alice", created)
	seq, marker, _, _, err := sr.store.Enqueue("ui:alice", "", "n", "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sr.store.MarkRun(seq); err != nil {
		t.Fatal(err)
	}
	items := []history.Item{
		{Role: "user", Text: "go", Marker: marker},
		{Role: "assistant", Text: "working"},
		{Role: "assistant", Text: "here is the answer"},
	}

	busy := testRM(d, created, items, true, true)
	if len(busy) != 1 || busy[0].State != "run" {
		t.Fatalf("busy newest run should be run: %+v", busy)
	}
	// Running: the last (in-progress) block is held, so only the earlier settled block shows and it
	// has a stable id. The dots stand in for the block still being generated.
	if len(busy[0].Blocks) != 1 || busy[0].Blocks[0].ID == "" || busy[0].Blocks[0].Role != "assistant" {
		t.Fatalf("running turn should show only settled blocks (last held): %+v", busy[0].Blocks)
	}
	// Idle (a missed MarkDone → resolved done): the turn is settled, so ALL blocks show — nothing
	// is held, and the final message appears exactly here, when the engine knows the turn is over.
	idle := testRM(d, created, items, false, true)
	if idle[0].State != "done" {
		t.Fatalf("idle run (missed MarkDone) must resolve to done, got %q", idle[0].State)
	}
	if len(idle[0].Blocks) != 2 {
		t.Fatalf("settled turn shows all blocks (no hold): %+v", idle[0].Blocks)
	}
}

func TestReadModelRunningKeepsToolProgressVisible(t *testing.T) {
	d, created := newReadModelDaemon(t)
	sr := d.getRunner("user:alice", created)
	seq, marker, _, _, err := sr.store.Enqueue("ui:alice", "", "n", "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sr.store.MarkRun(seq); err != nil {
		t.Fatal(err)
	}
	items := []history.Item{
		{Role: "user", Text: "go", Marker: marker},
		{Role: "assistant", Text: "settled"},
		{Role: "tool", Text: "🗜 Compaction: context compacted"},
	}

	turns := testRM(d, created, items, true, true)
	if len(turns) != 1 || turns[0].State != "run" {
		t.Fatalf("busy newest run should be run: %+v", turns)
	}
	if len(turns[0].Blocks) != 2 {
		t.Fatalf("running turn must keep tool progress visible: %+v", turns[0].Blocks)
	}
	if turns[0].Blocks[1].Role != "tool" || turns[0].Blocks[1].Text != "🗜 Compaction: context compacted" {
		t.Fatalf("last tool block was not preserved: %+v", turns[0].Blocks)
	}
}

// A markerless transcript turn (legacy, pre-durable-queue) renders done with a stable
// negative synthetic id that can never collide with a real durable seq (>= 1).
func TestReadModelLegacyMarkerless(t *testing.T) {
	d, created := newReadModelDaemon(t)
	items := []history.Item{{Role: "user", Text: "old"}, {Role: "assistant", Text: "reply"}}
	turns := testRM(d, created, items, false, true)
	if len(turns) != 1 || turns[0].Seq >= 0 || turns[0].State != "done" {
		t.Fatalf("legacy markerless turn: %+v", turns)
	}
}

func TestReadModelCarriesContextOnTurn(t *testing.T) {
	d, created := newReadModelDaemon(t)
	items := []history.Item{
		{Role: "user", Text: "u"},
		{Role: "assistant", Text: "a", CtxUsed: 144_000},
	}
	turns := testRM(d, created, items, false, true)
	if len(turns) != 1 || len(turns[0].Blocks) != 1 {
		t.Fatalf("context test model shape: %+v", turns)
	}
	// The per-turn "cut line" context comes from the last assistant block's usage; its window
	// falls back to the session window when the transcript carries none (Claude).
	if turns[0].CtxUsed != 144_000 || turns[0].CtxWindow != 1_000_000 {
		t.Fatalf("turn context = %d/%d, want 144000/1000000", turns[0].CtxUsed, turns[0].CtxWindow)
	}
}

// A codex turn whose final token_count lands on a trailing tool-only assistant item must
// still carry its context to the turn on reload — guards the tool-only branch of
// buildReadModel (the ctx capture lives outside the text-block `if`).
func TestReadModelContextFromToolOnlyBlock(t *testing.T) {
	d, created := newReadModelDaemon(t)
	items := []history.Item{
		{Role: "user", Text: "u"},
		{Role: "assistant", Tools: []history.ToolCall{{Name: "Exec", Label: "$ echo hi"}}, CtxUsed: 142_000, CtxWindow: 258_400},
	}
	turns := testRM(d, created, items, false, true)
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %+v", turns)
	}
	if turns[0].CtxUsed != 142_000 || turns[0].CtxWindow != 258_400 {
		t.Fatalf("tool-only turn context = %d/%d, want 142000/258400", turns[0].CtxUsed, turns[0].CtxWindow)
	}
}

// blockID is stable for identical content and includes the turn seq.
func TestBlockIDStable(t *testing.T) {
	if a, b := blockID(5, "assistant", "answer", nil), blockID(5, "assistant", "answer", nil); a != b || a == "" {
		t.Fatalf("blockID not stable: %q vs %q", a, b)
	}
	if blockID(5, "assistant", "answer", nil) == blockID(6, "assistant", "answer", nil) {
		t.Fatal("blockID must include the turn seq")
	}
	if blockID(5, "assistant", "answer", nil) == blockID(5, "assistant", "other", nil) {
		t.Fatal("blockID must include the text")
	}
}

// groupTurns nests answer/tool blocks under their user turn and keeps non-answer
// system notices as their own top-level unit.
func TestGroupTurns(t *testing.T) {
	g := groupTurns([]history.Item{
		{Role: "user", Text: "u1"},
		{Role: "assistant", Text: "a1"},
		{Role: "tool", Text: "t1"},
		{Role: "tool", Text: "compact"},
		{Role: "system", Kind: "error"},
		{Role: "user", Text: "u2"},
	})
	if len(g) != 3 {
		t.Fatalf("want 3 units (turn, notice, turn), got %d", len(g))
	}
	if g[0].lead.Role != "user" || len(g[0].blocks) != 3 {
		t.Fatalf("first unit should be a user turn with 3 blocks: %+v", g[0])
	}
	if g[1].lead.Role != "system" || len(g[1].blocks) != 0 {
		t.Fatalf("second unit should be a standalone notice: %+v", g[1])
	}
	if g[2].lead.Role != "user" || len(g[2].blocks) != 0 {
		t.Fatalf("third unit should be a user turn with no blocks yet: %+v", g[2])
	}
}

// An aborted queued turn (Last==err, never ran) is surfaced on reload with an error
// block — shown as stopped, not silently dropped or frozen.
func TestReadModelAbortedSurfaced(t *testing.T) {
	d, created := newReadModelDaemon(t)
	sr := d.getRunner("user:alice", created)
	seq, _, _, _, err := sr.store.Enqueue("ui:alice", "", "n", "doomed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sr.store.MarkErr(seq, "aborted"); err != nil {
		t.Fatal(err)
	}
	turns := testRM(d, created, nil, false, true)
	if len(turns) != 1 || turns[0].State != "err" {
		t.Fatalf("aborted turn not surfaced as err: %+v", turns)
	}
	if len(turns[0].Blocks) != 1 || turns[0].Blocks[0].Role != "error" {
		t.Fatalf("aborted turn missing its error block: %+v", turns[0].Blocks)
	}
	if turns[0].Blocks[0].Text != "Прервано" {
		t.Fatalf("aborted turn text = %q, want localized text", turns[0].Blocks[0].Text)
	}
}

func TestErrBlockCanonicalReasons(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{turnErrAborted, "Прервано"},
		{turnErrAttachmentsMissing, "Вложения недоступны, сообщение не обработано"},
		{turnErrRunStartFailed, "Не удалось зафиксировать запуск, сообщение не обработано"},
		{"backend failed", "backend failed"},
	}
	for _, tt := range tests {
		got := errBlock(11, tt.reason)
		if got.Text != tt.want {
			t.Fatalf("errBlock(%q).Text = %q, want %q", tt.reason, got.Text, tt.want)
		}
		if wantID := blockID(11, "error", tt.want, nil); got.ID != wantID {
			t.Fatalf("errBlock(%q).ID = %q, want %q", tt.reason, got.ID, wantID)
		}
	}
}

func TestReadModelAbortedKeepsTurnOrder(t *testing.T) {
	d, created := newReadModelDaemon(t)
	sr := d.getRunner("user:alice", created)
	seq1, marker1, _, _, err := sr.store.Enqueue("ui:alice", "", "n1", "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	seq2, _, _, _, err := sr.store.Enqueue("ui:alice", "", "n2", "aborted", nil)
	if err != nil {
		t.Fatal(err)
	}
	seq3, marker3, _, _, err := sr.store.Enqueue("ui:alice", "", "n3", "third", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{seq1, seq3} {
		if err := sr.store.MarkDone(seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := sr.store.MarkErr(seq2, "aborted"); err != nil {
		t.Fatal(err)
	}
	turns := testRM(d, created, []history.Item{
		{Role: "user", Text: "first", Marker: marker1},
		{Role: "assistant", Text: "one"},
		{Role: "user", Text: "third", Marker: marker3},
		{Role: "assistant", Text: "three"},
	}, false, true)
	if len(turns) != 3 {
		t.Fatalf("turn count = %d, want 3: %+v", len(turns), turns)
	}
	if turns[0].Seq != seq1 || turns[1].Seq != seq2 || turns[2].Seq != seq3 {
		t.Fatalf("turn order = [%d %d %d], want [%d %d %d]: %+v", turns[0].Seq, turns[1].Seq, turns[2].Seq, seq1, seq2, seq3, turns)
	}
	if turns[1].State != "err" {
		t.Fatalf("middle turn state = %q, want err: %+v", turns[1].State, turns[1])
	}
}

// blockID is canonical: a trailing-whitespace difference (live res.Text vs the trimmed
// transcript text) must NOT change the id, or the reload-race duplicate final can't dedup.
func TestBlockIDCanonical(t *testing.T) {
	if blockID(7, "assistant", "answer", nil) != blockID(7, "assistant", "answer\n\n ", nil) {
		t.Fatal("blockID must be canonical across trailing whitespace")
	}
}
