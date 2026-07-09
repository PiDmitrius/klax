package sessfiles

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A turn's files are named <turnSeq>-<NN>-name; duplicate names within one turn
// get distinct indices (no collision), and the bytes land.
func TestWriteTurnNamesAndBytes(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 1719500000)
	names, err := s.WriteTurn(42, []Blob{{"image.png", []byte("a")}, {"image.png", []byte("b")}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"000042-01-image.png", "000042-02-image.png"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if b, _ := os.ReadFile(s.Path(want[1])); string(b) != "b" {
		t.Fatalf("content = %q, want b", b)
	}
}

// Re-writing the same (turnSeq, idx, name) slot is a no-op (idempotent replay):
// same stored name, no error, original bytes intact.
func TestWriteFileIdempotentReplay(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 7)
	n1, err := s.WriteFile(3, 1, "doc.pdf", bytes.NewReader([]byte("orig")))
	if err != nil {
		t.Fatal(err)
	}
	n2, err := s.WriteFile(3, 1, "doc.pdf", bytes.NewReader([]byte("REPLAY")))
	if err != nil || n2 != n1 {
		t.Fatalf("replay name = %q (err %v), want %q", n2, err, n1)
	}
	if b, _ := os.ReadFile(s.Path(n1)); string(b) != "orig" {
		t.Fatalf("idempotent write clobbered bytes: %q", b)
	}
}

// Materialize gives the agent clean names (prefix stripped), disambiguates a
// within-turn clash, resolves to the durable bytes, and is a real copy (the
// run-view realpath stays in the run dir, not the internal store).
func TestMaterializeCleanNamesAndCollision(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 42)
	names, _ := s.WriteTurn(5, []Blob{{"shot.png", []byte("1")}, {"shot.png", []byte("2")}})
	run := filepath.Join(t.TempDir(), "klax-attach-x")
	paths, err := s.Materialize(names, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || filepath.Base(paths[0]) != "shot.png" || filepath.Base(paths[1]) != "shot-2.png" {
		t.Fatalf("clean view = %v, want [shot.png shot-2.png]", paths)
	}
	if c, _ := os.ReadFile(paths[1]); string(c) != "2" {
		t.Fatalf("clean view content = %q, want 2", c)
	}
	if fi, err := os.Lstat(paths[0]); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("run-view must be a real file, not a symlink (mode %v, err %v)", fi.Mode(), err)
	}
}

// keyDir is filesystem-safe and injective: keys that a char-replacement sanitizer
// would collide ("a:b" vs "a/b") map to distinct dirs, and none leaks a separator.
func TestKeyDirSafeAndInjective(t *testing.T) {
	for _, k := range []string{"user:alice", "tg:123/x", `mx:\weird`, "a:b", "a/b"} {
		if strings.ContainsAny(keyDir(k), "/:\\") {
			t.Fatalf("keyDir(%q)=%q leaked a path separator", k, keyDir(k))
		}
	}
	if keyDir("a:b") == keyDir("a/b") {
		t.Fatalf("keyDir not injective: a:b and a/b collide")
	}
	if !strings.HasSuffix(keyDir("user:alice"), base64.RawURLEncoding.EncodeToString([]byte("user:alice"))) {
		t.Fatalf("keyDir should end with the lossless base64url of the key")
	}
}

func TestRemove(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 7)
	if _, err := s.WriteFile(1, 1, "a.txt", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatalf("work dir should exist after a write: %v", err)
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("work dir should be gone after Remove")
	}
}

func TestSanitizeTraversalAndControl(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd": "passwd",
		`a\b\c.png`:        "c.png",
		"  spaced.txt  ":   "spaced.txt",
		"":                 "attachment",
		"..":               "attachment",
		"ok\x07bell.png":   "okbell.png",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayNameStripsDurablePrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"000042-01-photo.png":                          "photo.png",
		"out-c4b7c07d01f151fbfc0e52d9bedbf2c8-plan.md": "plan.md",
		"plain.txt": "plain.txt",
	} {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
