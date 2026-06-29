package history

import "testing"

func TestStripTurnMarker(t *testing.T) {
	const tok = "00112233445566ff" // 16 hex chars, the shape newMarker produces
	// The marker klax injects at the end of a prompt is stripped, its token returned.
	clean, marker := StripTurnMarker("hello world\n\n<!-- klax-turn:" + tok + " -->")
	if clean != "hello world" || marker != tok {
		t.Fatalf("got (%q, %q), want (hello world, %s)", clean, marker, tok)
	}
	// No marker: text unchanged, empty token.
	if c, m := StripTurnMarker("just text"); c != "just text" || m != "" {
		t.Fatalf("no-marker: got (%q, %q)", c, m)
	}
	// Tight (no spaces) trailing form is still matched.
	if c, m := StripTurnMarker("x<!--klax-turn:" + tok + "-->"); c != "x" || m != tok {
		t.Fatalf("tight form: got (%q, %q)", c, m)
	}
	// A wrong-length token (not our marker) is left intact.
	if c, m := StripTurnMarker("keep <!-- klax-turn:abc --> me"); c != "keep <!-- klax-turn:abc --> me" || m != "" {
		t.Fatalf("non-marker comment must be kept: got (%q, %q)", c, m)
	}
}
