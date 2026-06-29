package main

import (
	"os"
	"path/filepath"
	"testing"
)

// pathInRoots is a security boundary: only files genuinely inside a root may serve.
func TestPathInRoots(t *testing.T) {
	root := t.TempDir()
	files := filepath.Join(root, "files")
	if err := os.MkdirAll(files, 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(files, "a.png")
	if err := os.WriteFile(inside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if !pathInRoots(inside, root) {
		t.Fatal("a file inside the root must pass")
	}
	// A file outside the root must fail.
	outside := filepath.Join(t.TempDir(), "b.png")
	os.WriteFile(outside, []byte("y"), 0600)
	if pathInRoots(outside, root) {
		t.Fatal("a file outside every root must fail")
	}
	// A non-existent path fails (EvalSymlinks errors).
	if pathInRoots(filepath.Join(files, "nope"), root) {
		t.Fatal("a missing path must fail")
	}
	// An empty root never matches.
	if pathInRoots(inside, "") {
		t.Fatal("an empty root must not match")
	}
	// A symlink inside the root that escapes it resolves out and fails.
	link := filepath.Join(files, "evil")
	if err := os.Symlink(outside, link); err == nil {
		if pathInRoots(link, root) {
			t.Fatal("a symlink escaping the root must fail")
		}
	}
}
