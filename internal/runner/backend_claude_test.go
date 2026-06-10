package runner

import "testing"

func TestClaudeParseCompactBoundary(t *testing.T) {
	b := &ClaudeBackend{}
	evs, ok := b.ParseEvent([]byte(`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":1002497,"postTokens":8037}}`))
	if !ok || len(evs) != 1 {
		t.Fatalf("parse: ok=%v evs=%#v", ok, evs)
	}
	e := evs[0]
	if e.Type != EventCompact || e.Compact == nil {
		t.Fatalf("type=%v compact=%v", e.Type, e.Compact)
	}
	if e.Compact.Trigger != "auto" || e.Compact.PreTokens != 1002497 || e.Compact.PostTokens != 8037 {
		t.Fatalf("compact=%#v", e.Compact)
	}

	// A non-compact system line still parses as EventSystem, not EventCompact.
	evs2, ok2 := b.ParseEvent([]byte(`{"type":"system","subtype":"init","session_id":"s1","model":"m"}`))
	if !ok2 || len(evs2) != 1 || evs2[0].Type != EventSystem {
		t.Fatalf("init parse: ok=%v evs=%#v", ok2, evs2)
	}
}
