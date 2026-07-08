package main

import (
	"testing"

	"github.com/PiDmitrius/klax/internal/session"
)

// TestRaiseReadThroughIsMonotonic locks the durable read-watermark ingest (DURABLE_CURSOR_PLAN.md
// S2): a report only ever moves the watermark forward — a later turn, or the same turn with a
// further block — and a stale/duplicate/out-of-order report is a no-op, so nothing can un-read.
func TestRaiseReadThroughIsMonotonic(t *testing.T) {
	s := &session.Session{}

	if !raiseReadThrough(s, 42, 3) || s.ReadThroughTurn != 42 || s.ReadThroughBlock != 3 {
		t.Fatalf("initial raise: watermark = (%d,%d), want moved to (42,3)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
	if !raiseReadThrough(s, 42, 5) || s.ReadThroughBlock != 5 {
		t.Fatalf("same-turn further block: block = %d, want moved to 5", s.ReadThroughBlock)
	}
	if raiseReadThrough(s, 42, 5) {
		t.Fatal("re-report of the same position must be a no-op")
	}
	if raiseReadThrough(s, 42, 2) || s.ReadThroughBlock != 5 {
		t.Fatalf("earlier block on same turn regressed watermark to %d", s.ReadThroughBlock)
	}
	if raiseReadThrough(s, 41, 999) || s.ReadThroughTurn != 42 || s.ReadThroughBlock != 5 {
		t.Fatalf("earlier turn regressed watermark to (%d,%d)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
	if !raiseReadThrough(s, 43, 0) || s.ReadThroughTurn != 43 || s.ReadThroughBlock != 0 {
		t.Fatalf("later turn: watermark = (%d,%d), want moved to (43,0)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
}

// TestUnreadAfterCountsBlocksPastWatermark locks the badge count (DURABLE_CURSOR_PLAN.md S2):
// answer blocks (a turn's actions) strictly after (turn,block) count, a never-read (0,0) session is
// fully unread, user bubbles and standalone non-durable rows never count, and it drains to 0 as the
// watermark advances — so it is >0 exactly when a divider would show.
func TestUnreadAfterCountsBlocksPastWatermark(t *testing.T) {
	mkturn := func(seq int64, blocks int) uiTurn {
		u := uiTurn{Seq: seq, Role: "user"}
		for i := 0; i < blocks; i++ {
			u.Blocks = append(u.Blocks, uiBlock{Role: "assistant", Text: "b"})
		}
		return u
	}
	// turn 1 (2 actions), a standalone tool row (Seq 0), turn 2 (3 actions).
	page := []uiTurn{mkturn(1, 2), {Role: "tool", Text: "compact"}, mkturn(2, 3)}

	cases := []struct {
		name        string
		turn        int64
		block, want int
	}{
		{"never read → all blocks, standalone excluded", 0, 0, 5},
		{"read turn1 block0 → rest of turn1 + all turn2", 1, 0, 4},
		{"read turn1 fully → all turn2", 1, 1, 3},
		{"read into turn2 → one left", 2, 1, 1},
		{"read everything", 2, 2, 0},
	}
	for _, c := range cases {
		if got := unreadAfter(page, c.turn, c.block); got != c.want {
			t.Errorf("%s: unreadAfter(page, %d, %d) = %d, want %d", c.name, c.turn, c.block, got, c.want)
		}
	}
}

// TestTailFromSlicesPastCursor locks the live-tail slice (DURABLE_CURSOR_PLAN.md S3): a grown
// boundary turn, a state-changed boundary turn, or any turn past `head` yields the tail from the
// boundary turn (standalones ride along); an unchanged / fully-past cursor yields nil so the
// long-poll keeps holding; a stale/zero cursor resends the whole model.
func TestTailFromSlicesPastCursor(t *testing.T) {
	mkturn := func(seq int64, blocks int) uiTurn {
		u := uiTurn{Seq: seq, Role: "user"}
		for i := 0; i < blocks; i++ {
			u.Blocks = append(u.Blocks, uiBlock{Role: "assistant", Text: "b"})
		}
		return u
	}
	// turn 1 (2 blocks), a standalone tool row, turn 2 (1 block). mkturn leaves State "" (code "e"),
	// so passing "e" keeps the state check neutral and exercises the block/new-turn logic.
	page := []uiTurn{mkturn(1, 2), {Role: "tool", Text: "compact"}, mkturn(2, 1)}

	if got := tailFrom(page, 1, 0, "e", 0, 1); len(got) != 3 || got[0].Seq != 1 {
		t.Fatalf("boundary turn grew: got %d rows (want 3 from turn 1)", len(got))
	}
	if got := tailFrom(page, 1, 1, "e", 0, 1); len(got) != 3 || got[0].Seq != 1 {
		t.Fatalf("later turn is new: got %d rows (want 3 from turn 1)", len(got))
	}
	if got := tailFrom(page, 2, 0, "e", 0, 2); got != nil {
		t.Fatalf("nothing new past (2,0,e,0): got %d rows, want nil", len(got))
	}
	// A cursor block PAST the boundary turn's current block count — the turn shrank, or a stale
	// over-read — re-syncs the boundary turn so live can't keep stale extra blocks a reload wouldn't.
	if got := tailFrom(page, 2, 5, "e", 0, 2); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("shrink/over-read must re-sync the boundary turn: got %d rows, want 1 from turn 2", len(got))
	}
	if got := tailFrom(page, 0, 0, "", 0, 0); len(got) != 3 {
		t.Fatalf("zero cursor resends all: got %d rows, want 3", len(got))
	}

	// The boundary (latest) turn flips enq→run with NO new block (backend still "thinking"): the state
	// code makes it fresh ONCE, then it holds (no spin) once the client has acked the run state.
	running := []uiTurn{{Seq: 1, Role: "user", State: "run"}} // started, no answer block yet
	if got := tailFrom(running, 1, -1, "e", 0, 1); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("enq→run with no new block must re-deliver once: got %d rows, want 1", len(got))
	}
	if got := tailFrom(running, 1, -1, "r", 0, 1); got != nil {
		t.Fatalf("already-acked run state must hold (no spin): got %d rows, want nil", len(got))
	}

	// A standalone non-durable row appended AFTER the last durable turn has no durable position of its
	// own — the trail count (cursor 4th field, 0→1) delivers it once from the boundary, then holds.
	trailing := []uiTurn{mkturn(4, 1), {Role: "tool", Text: "compact"}}
	if got := tailFrom(trailing, 4, 0, "e", 0, 4); len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("trailing standalone must deliver once from the boundary: got %d rows, want 2 from turn 4", len(got))
	}
	if got := tailFrom(trailing, 4, 0, "e", 1, 4); got != nil {
		t.Fatalf("already-acked trailing standalone must hold (no spin): got %d rows, want nil", len(got))
	}

	// QUEUE: ping #1 still RUNNING while ping #2 is enqueued behind it. The cursor anchors on the
	// running turn 1 (block 0, run) but carries head=2, so the already-seen queued turn 2 is not
	// re-flagged as "new" — this is the regression guard for the "first turn's answer never arrives"
	// bug where the cursor had jumped to turn 2 and turn 1's completion became invisible.
	q1run := []uiTurn{{Seq: 1, Role: "user", State: "run", Blocks: []uiBlock{{Role: "assistant", Text: "b"}}}, {Seq: 2, Role: "user", State: "enq"}}
	if got := tailFrom(q1run, 1, 0, "r", 0, 2); got != nil {
		t.Fatalf("running turn behind a queued one, nothing new: got %d rows, want nil (no spin)", len(got))
	}
	// ping #1 COMPLETES (run→done): delivered from turn 1 even though a newer turn (2) sits ahead.
	q1done := []uiTurn{{Seq: 1, Role: "user", State: "done", Blocks: []uiBlock{{Role: "assistant", Text: "b"}}}, {Seq: 2, Role: "user", State: "enq"}}
	if got := tailFrom(q1done, 1, 0, "r", 0, 2); len(got) != 2 || got[0].Seq != 1 {
		t.Fatalf("a running turn's completion behind a queued turn must deliver: got %d rows, want 2 from turn 1", len(got))
	}
	// a genuinely NEW turn 3 (past head) still fires, delivering from the anchor.
	q3 := append(append([]uiTurn{}, q1run...), uiTurn{Seq: 3, Role: "user", State: "enq"})
	if got := tailFrom(q3, 1, 0, "r", 0, 2); len(got) != 3 || got[0].Seq != 1 {
		t.Fatalf("a turn past head must deliver from the anchor: got %d rows, want 3 from turn 1", len(got))
	}
}

// TestBlockCursorRoundTrips locks the tail cursor wire format
// "<turn>.<block>.<state>.<trail>[.<head>]": tailCursor stamps the anchor turn's seq/last-block/state
// code, parseBlockCursor reverses it, and a legacy "<turn>.<block>" cursor parses to an empty state
// so it re-syncs once rather than erroring.
func TestBlockCursorRoundTrips(t *testing.T) {
	cur := tailCursor([]uiTurn{{Seq: 7, Role: "user", State: "run"}}) // 0 blocks, no trailing rows
	if cur != "7.-1.r.0" {
		t.Fatalf("tailCursor(run, 0 blocks) = %q, want 7.-1.r.0", cur)
	}
	if turn, block, state, trail, head := parseBlockCursor(cur); turn != 7 || block != -1 || state != "r" || trail != 0 || head != 7 {
		t.Fatalf("parseBlockCursor(%q) = (%d,%d,%q,%d,%d), want (7,-1,r,0,7)", cur, turn, block, state, trail, head)
	}
	// two answer blocks (done) + one trailing standalone → block 1, trail 1 (all settled ⇒ 4-segment)
	if cur := tailCursor([]uiTurn{{Seq: 3, Role: "user", State: "done", Blocks: []uiBlock{{}, {}}}, {Role: "tool", Text: "compact"}}); cur != "3.1.d.1" {
		t.Fatalf("tailCursor(done, 2 blocks, 1 trailing) = %q, want 3.1.d.1", cur)
	}
	// QUEUE: a RUNNING turn 1 (1 block) behind an ENQUEUED turn 2 → 5-segment cursor anchored on the
	// running turn with head=2, so the cursor never advances past the still-running turn.
	if cur := tailCursor([]uiTurn{{Seq: 1, Role: "user", State: "run", Blocks: []uiBlock{{}}}, {Seq: 2, Role: "user", State: "enq"}}); cur != "1.0.r.0.2" {
		t.Fatalf("tailCursor(run turn 1 behind enq turn 2) = %q, want 1.0.r.0.2", cur)
	}
	if turn, block, state, trail, head := parseBlockCursor("1.0.r.0.2"); turn != 1 || block != 0 || state != "r" || trail != 0 || head != 2 {
		t.Fatalf("parseBlockCursor(5-seg) = (%d,%d,%q,%d,%d), want (1,0,r,0,2)", turn, block, state, trail, head)
	}
	// Legacy cursors (a tab open from before the state/trail/head fields) parse the missing fields as
	// zero-values (head defaults to the anchor turn) so they re-sync once rather than erroring.
	if turn, block, state, trail, head := parseBlockCursor("5.0"); turn != 5 || block != 0 || state != "" || trail != 0 || head != 5 {
		t.Fatalf(`parseBlockCursor("5.0") = (%d,%d,%q,%d,%d), want (5,0,"",0,5)`, turn, block, state, trail, head)
	}
	if turn, block, state, trail, head := parseBlockCursor("5.0.r"); turn != 5 || block != 0 || state != "r" || trail != 0 || head != 5 {
		t.Fatalf(`parseBlockCursor("5.0.r") = (%d,%d,%q,%d,%d), want (5,0,"r",0,5)`, turn, block, state, trail, head)
	}
	if turn, block, state, trail, head := parseBlockCursor(""); turn != 0 || block != -1 || state != "" || trail != 0 || head != 0 {
		t.Fatalf(`parseBlockCursor("") = (%d,%d,%q,%d,%d), want (0,-1,"",0,0)`, turn, block, state, trail, head)
	}
}
