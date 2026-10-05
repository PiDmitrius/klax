package main

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/session"
)

// TestRaiseReadPosIsMonotonic locks the durable read-position ingest: a report only ever moves the
// position forward — a later turn, or the same turn with a further block — and a stale, duplicate
// or out-of-order report is a no-op, so nothing can un-read.
func TestRaiseReadPosIsMonotonic(t *testing.T) {
	s := &session.Session{}
	steps := []struct {
		turn   int64
		block  int
		raised bool
		want   string
	}{
		{42, 3, true, "42.3"},
		{42, 5, true, "42.5"},
		{42, 5, false, "42.5"},
		{42, 2, false, "42.5"},
		{41, 999, false, "42.5"},
		{43, 0, true, "43.0"},
	}
	for _, st := range steps {
		if got := s.AdvanceReadPos(false, st.turn, st.block); got != st.raised || s.ReadPos != st.want {
			t.Fatalf("AdvanceReadPos(%d,%d) = %v, read_pos %q; want %v, %q", st.turn, st.block, got, s.ReadPos, st.raised, st.want)
		}
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
	groups := groupRows("s9", rows)
	var keys []string
	for i, g := range groups {
		keys = append(keys, g.Key)
		if i > 0 && !groups[i-1].Ord.less(g.Ord) {
			t.Fatalf("ord not increasing at %d: %+v then %+v", i, groups[i-1].Ord, g.Ord)
		}
	}
	if want := []string{"t:0", "t:1", "t:2", "t:3", "t:4"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if len(groups[0].Rows) != 1 || groups[0].Head != nil || len(groups[1].Rows) != 1 || groups[1].Rows[0].Text != "note" {
		t.Fatalf("standalone rows misplaced: %+v", groups)
	}
	for i, want := range []string{`"0.-1"`, `"1.3"`, `"2.8"`, `"3.8"`, `"4"`} {
		if got, _ := json.Marshal(groups[i].Ord); string(got) != want {
			t.Fatalf("ord %d = %s, want %s", i, got, want)
		}
	}
	if o, ok := parseBound("3.8"); !ok || o != groups[3].Ord {
		t.Fatalf("parseBound(3.8) = %+v, %v", o, ok)
	}
	if _, ok := parseBound("4"); ok {
		t.Fatal("a bound without a transcript record must be rejected")
	}
}

// applyMerge applies a merge patch to a wire object, as the client does.
func applyMerge(base []byte, patch json.RawMessage) []byte {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(base, &m)
	var p map[string]json.RawMessage
	_ = json.Unmarshal(patch, &p)
	for k, v := range p {
		if string(v) == "null" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	out, _ := json.Marshal(m)
	return out
}

func applyArray[T any](xs []T, d *uiArrayDelta) []T {
	if d == nil {
		return xs
	}
	w := wireList(xs)
	for i, p := range d.Set {
		n, _ := strconv.Atoi(i)
		w[n] = applyMerge(w[n], p)
	}
	if d.Append != nil {
		w = w[:d.Start]
	}
	for _, e := range d.Append {
		w = append(w, e)
	}
	if d.Length != nil {
		w = w[:*d.Length]
	}
	out := make([]T, len(w))
	for i, e := range w {
		_ = json.Unmarshal(e, &out[i])
	}
	return out
}

// applyDelta is the client's rule for one group event.
func applyDelta(g *uiGroup, d *uiGroupDelta) {
	g.Ord = d.Ord
	if d.Create != nil {
		g.Head, g.Blocks, g.Rows = d.Create.Head, d.Create.Blocks, d.Create.Rows
		return
	}
	if d.Head != nil {
		hb, _ := json.Marshal(g.Head)
		var h uiTurn
		_ = json.Unmarshal(applyMerge(hb, d.Head), &h)
		g.Head = &h
	}
	g.Blocks, g.Rows = applyArray(g.Blocks, d.Blocks), applyArray(g.Rows, d.Rows)
}

func wireEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A changed group sends only the fields and blocks that changed: a header patch, element patches
// by index, appended blocks and a shorter length; a new group is created in full, a vanished one is
// removed after the group events, and replaying the deltas reproduces the new value.
func TestDiffGroupsSendsOnlyChanges(t *testing.T) {
	block := func(text string) uiBlock { return uiBlock{Role: "assistant", Text: text} }
	head := func(state string) *uiTurn { return &uiTurn{Role: "user", Seq: 5, Text: "long prompt", State: state} }
	old := []uiGroup{
		{Key: "t:1:4", Ord: uiOrd{event: 1, seq: 4}, Head: &uiTurn{Role: "user", Seq: 4, State: "done"}},
		{Key: "t:1:5", Ord: uiOrd{event: 2, seq: 5}, Head: head("run"), Blocks: []uiBlock{block("A"), block("B"), block("C")}},
		{Key: "t:1:-9", Ord: uiOrd{event: 3, seq: -9}, Head: &uiTurn{Role: "user", Seq: -9}},
	}
	next := head("done")
	next.CtxUsed = 900
	cur := []uiGroup{
		old[0],
		{Key: "t:1:5", Ord: uiOrd{event: 2, seq: 5}, Head: next, Blocks: []uiBlock{block("A"), {Role: "assistant", Text: "B", Time: "t1"}, block("C"), block("D")}},
		{Key: "t:1:6", Ord: uiOrd{event: 3, seq: 6}, Head: &uiTurn{Role: "user", Seq: 6, State: "done"}},
	}
	evs := diffGroups("s1", old, cur)
	if len(evs) != 3 || evs[0].ev.Group == nil || evs[1].ev.Group == nil || evs[2].ev.Removed == nil {
		t.Fatalf("events = %+v, want two group deltas then one removal", evs)
	}
	d := evs[0].ev.Group
	if string(d.Head) != `{"ctx_used":900,"state":"done"}` {
		t.Fatalf("head patch = %s, want only the changed fields", d.Head)
	}
	if d.Blocks == nil || len(d.Blocks.Set) != 1 || string(d.Blocks.Set["1"]) != `{"time":"t1"}` || len(d.Blocks.Append) != 1 || d.Blocks.Start != 3 || d.Blocks.Length != nil {
		t.Fatalf("blocks delta = %+v, want one field patch and one appended block", d.Blocks)
	}
	g := old[1]
	applyDelta(&g, d)
	if !wireEqual(g, cur[1]) {
		t.Fatalf("replayed group = %+v, want %+v", g, cur[1])
	}
	if n := evs[1].ev.Group; n.Key != "t:1:6" || n.Create == nil || n.Create.Head == nil {
		t.Fatalf("new group delta = %+v", n)
	}
	if rm := evs[2].ev.Removed; rm.Key != "t:1:-9" || rm.Ord != old[2].Ord {
		t.Fatalf("removal = %+v", rm)
	}
	shrunk := []uiGroup{old[0], {Key: "t:1:5", Ord: old[1].Ord, Head: old[1].Head, Blocks: old[1].Blocks[:1]}, old[2]}
	evs = diffGroups("s1", old, shrunk)
	if len(evs) != 1 || evs[0].ev.Group.Head != nil || evs[0].ev.Group.Blocks.Length == nil || *evs[0].ev.Group.Blocks.Length != 1 {
		t.Fatalf("shrunk group delta = %+v", evs)
	}
	if evs := diffGroups("s1", old, old); len(evs) != 0 {
		t.Fatalf("unchanged groups produced %d events", len(evs))
	}
}

// A tab patch carries only the changed fields (null for a dropped one); a new tab arrives whole
// and membership changes send the order.
func TestDiffTabsSendsOnlyChanges(t *testing.T) {
	a := uiSessionInfo{KlaxID: "a1", Name: "one", Unread: 3, ReadPos: "1.0"}
	b := uiSessionInfo{KlaxID: "b2", Name: "two"}
	pub, evs := diffTabs(roleRW, nil, []uiSessionInfo{a, b})
	if len(evs) != 0 {
		t.Fatalf("first publication sent %d events", len(evs))
	}
	a.Unread, a.ReadPos = 0, "2.0"
	c := uiSessionInfo{KlaxID: "c3", Name: "three"}
	_, evs = diffTabs(roleRW, pub, []uiSessionInfo{a, c})
	if len(evs) != 3 {
		t.Fatalf("events = %+v, want two tab patches and the order", evs)
	}
	if got := string(evs[0].ev.Tab); got != `{"klax_id":"a1","read_pos":"2.0","unread":null}` {
		t.Fatalf("tab patch = %s", got)
	}
	if !strings.Contains(string(evs[1].ev.Tab), `"name":"three"`) || !slices.Equal(evs[2].ev.Tabs, []string{"a1", "c3"}) || evs[2].role != roleRW {
		t.Fatalf("new tab and order = %s %v", evs[1].ev.Tab, evs[2].ev.Tabs)
	}
}
