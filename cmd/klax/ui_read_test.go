package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/PiDmitrius/klax/internal/session"
)

// TestRaiseReadThroughIsMonotonic locks the durable read-watermark ingest: a report only ever
// moves the watermark forward — a later turn, or the same turn with a
// further block — and a stale/duplicate/out-of-order report is a no-op, so nothing can un-read.
func TestRaiseReadThroughIsMonotonic(t *testing.T) {
	s := &session.Session{}

	if !s.AdvanceReadThrough(false, 42, 3) || s.ReadThroughTurn != 42 || s.ReadThroughBlock != 3 {
		t.Fatalf("initial raise: watermark = (%d,%d), want moved to (42,3)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
	if !s.AdvanceReadThrough(false, 42, 5) || s.ReadThroughBlock != 5 {
		t.Fatalf("same-turn further block: block = %d, want moved to 5", s.ReadThroughBlock)
	}
	if s.AdvanceReadThrough(false, 42, 5) {
		t.Fatal("re-report of the same position must be a no-op")
	}
	if s.AdvanceReadThrough(false, 42, 2) || s.ReadThroughBlock != 5 {
		t.Fatalf("earlier block on same turn regressed watermark to %d", s.ReadThroughBlock)
	}
	if s.AdvanceReadThrough(false, 41, 999) || s.ReadThroughTurn != 42 || s.ReadThroughBlock != 5 {
		t.Fatalf("earlier turn regressed watermark to (%d,%d)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
	if !s.AdvanceReadThrough(false, 43, 0) || s.ReadThroughTurn != 43 || s.ReadThroughBlock != 0 {
		t.Fatalf("later turn: watermark = (%d,%d), want moved to (43,0)", s.ReadThroughTurn, s.ReadThroughBlock)
	}
}

// TestUnreadAfterCountsBlocksPastWatermark locks the badge count:
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
	cancelled := append(page, uiTurn{Seq: 3, Role: "user", State: "err", Blocks: []uiBlock{{Role: "system", Kind: "cancelled"}}})
	if got := unreadAfter(cancelled, 2, 2); got != 0 {
		t.Errorf("a cancelled note counts as unread: %d", got)
	}
}

// Turn groups get stable keys and strictly increasing ords: standalone rows join the preceding
// user row (group 0 before the first), and a queue-only turn sorts right before the transcript
// turn it is placed before, or last.
func TestGroupRowsKeysAndOrd(t *testing.T) {
	rows := []uiTurn{
		{Role: "system", Text: "intro", event: 0},
		{Role: "user", Seq: 1, event: 3},
		{Role: "system", Text: "note", event: 4},
		{Role: "user", Seq: 2, queueOnly: true},
		{Role: "user", Seq: 3, event: 8},
		{Role: "user", Seq: 4, queueOnly: true},
	}
	groups := groupRows(9, rows)
	var keys []string
	for i, g := range groups {
		keys = append(keys, g.Key)
		if i > 0 && !groups[i-1].Ord.less(g.Ord) {
			t.Fatalf("ord not increasing at %d: %+v then %+v", i, groups[i-1].Ord, g.Ord)
		}
	}
	if want := []string{"t:9:0", "t:9:1", "t:9:2", "t:9:3", "t:9:4"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if len(groups[0].Rows) != 1 || groups[0].Head != nil || len(groups[1].Rows) != 1 || groups[1].Rows[0].Text != "note" {
		t.Fatalf("standalone rows misplaced: %+v", groups)
	}
	for i, want := range []string{"[-1,0]", "[3,1]", "[8,2]", "[8,3]", "[null,4]"} {
		if got, _ := json.Marshal(groups[i].Ord); string(got) != want {
			t.Fatalf("ord %d = %s, want %s", i, got, want)
		}
	}
	if o, ok := parseOrd("null,4"); !ok || o != groups[4].Ord {
		t.Fatalf("parseOrd(null,4) = %+v, %v", o, ok)
	}
}

// applyDelta is the client's rule for one group event.
func applyDelta(g *uiGroup, d *uiGroupDelta) {
	g.Ord = d.Ord
	if d.Head != nil {
		g.Head = d.Head
	} else if d.Ctx != nil {
		h := *g.Head
		h.CtxUsed, h.CtxWindow = d.Ctx[0], d.Ctx[1]
		g.Head = &h
	}
	g.Blocks = append(slices.Clone(g.Blocks[:d.From]), d.Blocks...)[:d.N]
	if d.Rows != nil {
		g.Rows = *d.Rows
	}
}

// A changed group sends only its changed block suffix and its header only when it changed; a
// vanished group is removed after the group events; replaying the deltas reproduces the new value.
func TestDiffGroupsSendsChangedSuffix(t *testing.T) {
	block := func(id, text string) uiBlock { return uiBlock{ID: id, Role: "assistant", Text: text} }
	head := func(state string) *uiTurn { return &uiTurn{Role: "user", Seq: 5, State: state} }
	old := []uiGroup{
		{Key: "t:1:4", Ord: uiOrd{event: 1, seq: 4}, Head: &uiTurn{Role: "user", Seq: 4, State: "done"}},
		{Key: "t:1:5", Ord: uiOrd{event: 2, seq: 5}, Head: head("run"), Blocks: []uiBlock{block("a", "A"), block("b", "B")}},
		{Key: "t:1:-9", Ord: uiOrd{event: 3, seq: -9}, Head: &uiTurn{Role: "user", Seq: -9}},
	}
	cur := []uiGroup{
		old[0],
		{Key: "t:1:5", Ord: uiOrd{event: 2, seq: 5}, Head: head("done"), Blocks: []uiBlock{block("a", "A"), block("b", "B2"), block("c", "C")}},
		{Key: "t:1:6", Ord: uiOrd{event: 3, seq: 6}, Head: &uiTurn{Role: "user", Seq: 6, State: "done"}},
	}
	evs := diffGroups(1, old, cur)
	if len(evs) != 3 || evs[0].ev.Group == nil || evs[1].ev.Group == nil || evs[2].ev.Removed == nil {
		t.Fatalf("events = %+v, want two group deltas then one removal", evs)
	}
	d := evs[0].ev.Group
	if d.Key != "t:1:5" || d.From != 1 || d.N != 3 || len(d.Blocks) != 2 || d.Head == nil || d.Head.State != "done" {
		t.Fatalf("changed group delta = %+v", d)
	}
	g := old[1]
	applyDelta(&g, d)
	if !reflect.DeepEqual(g.Head, cur[1].Head) || !reflect.DeepEqual(g.Blocks, cur[1].Blocks) {
		t.Fatalf("replayed group = %+v, want %+v", g, cur[1])
	}
	if n := evs[1].ev.Group; n.Key != "t:1:6" || n.From != 0 || n.Head == nil {
		t.Fatalf("new group delta = %+v", n)
	}
	if rm := evs[2].ev.Removed; rm.Key != "t:1:-9" || rm.Ord != old[2].Ord {
		t.Fatalf("removal = %+v", rm)
	}
	shrunk := []uiGroup{old[0], {Key: "t:1:5", Ord: old[1].Ord, Head: old[1].Head, Blocks: old[1].Blocks[:1]}, old[2]}
	evs = diffGroups(1, old, shrunk)
	if len(evs) != 1 || evs[0].ev.Group.From != 1 || evs[0].ev.Group.N != 1 || evs[0].ev.Group.Head != nil {
		t.Fatalf("shrunk group delta = %+v", evs)
	}
	used := []uiGroup{old[0], {Key: "t:1:5", Ord: old[1].Ord, Head: &uiTurn{Role: "user", Seq: 5, State: "run", CtxUsed: 900}, Blocks: old[1].Blocks}, old[2]}
	evs = diffGroups(1, old, used)
	if len(evs) != 1 || evs[0].ev.Group.Head != nil || evs[0].ev.Group.Ctx == nil || *evs[0].ev.Group.Ctx != [2]int{900, 0} {
		t.Fatalf("context-only delta = %+v, want ctx without the head", evs)
	}
	if evs := diffGroups(1, old, old); len(evs) != 0 {
		t.Fatalf("unchanged groups produced %d events", len(evs))
	}
}
