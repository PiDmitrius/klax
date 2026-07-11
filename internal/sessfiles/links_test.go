package sessfiles

import (
	"os"
	"testing"
)

func TestEnsureLinkStablePersistedAndRemoved(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 1)

	tok, err := s.EnsureLink("000001-01-a.png", "a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 22 {
		t.Fatalf("token should be 22 base64url chars, got %d: %q", len(tok), tok)
	}
	// Same file → same token (stable across rebuilds).
	if again, err := s.EnsureLink("000001-01-a.png", "a.png", "image/png"); err != nil || again != tok {
		t.Fatalf("EnsureLink must be stable: %q vs %q (err %v)", again, tok, err)
	}
	// A fresh Store (a restart) reads the SAME token + metadata from links.json.
	links, err := Open("user:alice", 1).Links()
	if err != nil {
		t.Fatal(err)
	}
	if e := links["000001-01-a.png"]; e.Token != tok || e.Name != "a.png" || e.ContentType != "image/png" {
		t.Fatalf("link not persisted correctly: %+v", e)
	}
	// A different file gets a distinct token.
	if other, err := s.EnsureLink("000001-01-b.png", "b.png", "image/png"); err != nil || other == tok {
		t.Fatalf("distinct files must get distinct tokens: %q vs %q (err %v)", other, tok, err)
	}
	// After Remove, EnsureLink refuses (no resurrection) and the file is gone with the dir.
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureLink("000001-01-c.png", "c.png", "image/png"); err != ErrRemoved {
		t.Fatalf("EnsureLink after Remove = %v, want ErrRemoved", err)
	}
	if _, err := os.Stat(s.linksPath()); !os.IsNotExist(err) {
		t.Fatalf("links.json must be removed with the session dir: %v", err)
	}
}
