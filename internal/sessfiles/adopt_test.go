package sessfiles

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func adoptTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{dir: t.TempDir()}
}

// The same bytes always land on the same blob name.
func TestAdoptIsContentAddressedAndIdempotent(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}

	first, _, err := s.Adopt("out.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Adopt("out.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same content adopted as %q then %q", first, second)
	}
	if _, err := os.Stat(s.Path(first)); err != nil {
		t.Fatalf("stored copy missing: %v", err)
	}
}

// The name follows content, not metadata: same size and mtime, different bytes, different blob.
func TestAdoptFollowsContentNotMetadata(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(src, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Adopt("out.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("after!"), 0600); err != nil { // same length
		t.Fatal(err)
	}
	if err := os.Chtimes(src, fi.ModTime(), fi.ModTime()); err != nil { // and the same mtime
		t.Fatal(err)
	}
	second, _, err := s.Adopt("out.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changed content reused the previous stored name — the name is not content-addressed")
	}
	for _, stored := range []string{first, second} {
		if _, err := os.Stat(s.Path(stored)); err != nil {
			t.Fatalf("stored copy %q missing: %v", stored, err)
		}
	}
}

// adoptAndCommit is the production sequence: adopt, then commit the link in one write.
func adoptAndCommit(t *testing.T, s *Store, name, src string) string {
	t.Helper()
	blob, fi, err := s.Adopt(name, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(LinkRecord{Blob: blob, Name: name, SeenPath: src, SeenInfo: fi}); err != nil {
		t.Fatal(err)
	}
	return blob
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so the permission assertion would be vacuous")
	}
}

// Repeat adoptions of one unchanged file converge on one blob and write nothing.
func TestAdoptRepeatStoresOneBlobAndWritesNothingTwice(t *testing.T) {
	skipIfRoot(t)
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	first, _, err := s.Adopt("report.csv", src)
	if err != nil {
		t.Fatal(err)
	}

	// Unwritable blob directory: any path that stages a temp copy fails here. A blob count alone
	// cannot see the difference.
	if err := os.Chmod(s.filesDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(s.filesDir(), 0o700)

	second, _, err := s.Adopt("report.csv", src)
	if err != nil {
		t.Fatalf("the repeat tried to write: %v", err)
	}
	if second != first {
		t.Fatalf("unchanged content produced a second blob: %q then %q", first, second)
	}
}

// An unchanged file is not read at all. Counted, not inferred: the result is identical either way,
// and a permission-based proof would depend on the filesystem's timestamp granularity.
func TestAdoptSkipsRereadingAnUnchangedFile(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Past the racy window: a just-written file is not trusted.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	first := adoptAndCommit(t, s, "report.csv", src)

	before := sourceProbes.Load()
	second, _, err := s.Adopt("report.csv", src)
	if err != nil {
		t.Fatal(err)
	}
	if n := sourceProbes.Load() - before; n != 0 {
		t.Fatalf("the unchanged file was read %d time(s)", n)
	}
	if second != first {
		t.Fatalf("unchanged file adopted as %q then %q", first, second)
	}
}

// Each mutation changes at least one of (size, mtime, inode), so the index must miss.
func TestAdoptIdentityIndexNoticesRealChanges(t *testing.T) {
	age := func(t *testing.T, p string) {
		t.Helper()
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, src string){
		"content rewritten in place": func(t *testing.T, src string) {
			if err := os.WriteFile(src, []byte("wholly different content"), 0600); err != nil {
				t.Fatal(err)
			}
			age(t, src)
		},
		"replaced by another file at the same path": func(t *testing.T, src string) {
			other := filepath.Join(filepath.Dir(src), "other.bin")
			if err := os.WriteFile(other, []byte("aaaaaaaa"), 0600); err != nil { // same length as the original
				t.Fatal(err)
			}
			fi, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(other, fi.ModTime(), fi.ModTime()); err != nil { // and the same mtime
				t.Fatal(err)
			}
			if err := os.Rename(other, src); err != nil { // only the inode differs
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := adoptTestStore(t)
			src := filepath.Join(t.TempDir(), "out.bin")
			if err := os.WriteFile(src, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			age(t, src)
			first := adoptAndCommit(t, s, "out.bin", src)

			mutate(t, src)
			second, _, err := s.Adopt("out.bin", src)
			if err != nil {
				t.Fatal(err)
			}
			if second == first {
				t.Fatal("a changed file was served from the identity index")
			}
		})
	}
}

// A file written moments ago is refused by the index.
func TestAdoptDoesNotTrustAFreshlyWrittenFile(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "fresh.bin")
	if err := os.WriteFile(src, []byte("just written"), 0600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	blob, _, err := s.Adopt("fresh.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(LinkRecord{Blob: blob, SeenPath: src, SeenInfo: fi}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.SeenBlob(src, fi); ok {
		t.Fatal("a file written within the racy window was accepted as a settled identity")
	}
}

func TestAdoptAfterRemoveStillFails(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Adopt("out.bin", src); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Adopt("out.bin", src); err != ErrRemoved {
		t.Fatalf("adopt on a removed store: err=%v, want ErrRemoved", err)
	}
}

// The mapping is durable across Store instances and stops answering once its blob is gone.
func TestSourceMappingIsDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n"), 0600); err != nil {
		t.Fatal(err)
	}

	s := &Store{dir: dir}
	stored, _, err := s.Adopt("report.csv", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(LinkRecord{Blob: stored, Source: src}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	fresh := &Store{dir: dir} // a restart
	got, ok := fresh.SourceStored(src)
	if !ok || got != stored {
		t.Fatalf("mapping did not survive: got=%q ok=%v, want %q", got, ok, stored)
	}

	if err := os.Remove(fresh.Path(stored)); err != nil {
		t.Fatal(err)
	}
	if _, ok := (&Store{dir: dir}).SourceStored(src); ok {
		t.Fatal("mapping still answered after its stored copy was deleted")
	}
}

// Recording a mapping leaves the token index in the same file untouched.
func TestRecordSourceKeepsTokens(t *testing.T) {
	s := adoptTestStore(t)
	token, err := s.Commit(LinkRecord{Blob: "out-abc-report.csv", Name: "report.csv", ContentType: "text/csv"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(LinkRecord{Blob: "out-abc-report.csv", Source: "/tmp/report.csv"}); err != nil {
		t.Fatal(err)
	}
	again, err := (&Store{dir: s.dir}).Commit(LinkRecord{Blob: "out-abc-report.csv", Name: "report.csv", ContentType: "text/csv"})
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Fatalf("token changed across a source recording: %q → %q", token, again)
	}
}

// A whole link is one durable write: token, turn mapping and content identity together. Writes are
// counted by links.json's INODE — every write publishes a fresh file by rename, so the inode changes
// exactly once per write. mtime and size cannot see this: an identical rewrite keeps both.
func TestCommitWritesLinksJSONOnce(t *testing.T) {
	s := adoptTestStore(t)
	src := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour) // past the racy window, so the identity is usable below
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	blob, fi, err := s.Adopt("report.csv", src)
	if err != nil {
		t.Fatal(err)
	}

	inode := func() uint64 {
		st, err := os.Stat(s.linksPath())
		if err != nil {
			return 0 // absent
		}
		dev, ino, _, ok := fileIdentity(st)
		if !ok {
			t.Skip("no file identity on this filesystem")
		}
		_ = dev
		return ino
	}
	if inode() != 0 {
		t.Fatal("Adopt wrote links.json; the caller owns that write")
	}

	rec := LinkRecord{
		Blob: blob, Name: "report.csv", ContentType: "text/csv",
		Source: "7:" + src, SeenPath: src, SeenInfo: fi,
	}
	before := linksWrites.Load()
	if _, err := s.Commit(rec); err != nil {
		t.Fatal(err)
	}
	if n := linksWrites.Load() - before; n != 1 {
		t.Fatalf("one link took %d writes of links.json, want exactly 1", n)
	}
	first := inode()
	if first == 0 {
		t.Fatal("links.json missing after the commit")
	}

	// All three maps landed in that single write.
	fresh := &Store{dir: s.dir}
	if got, ok := fresh.SourceStored("7:" + src); !ok || got != blob {
		t.Fatalf("source mapping missing after one write: %q ok=%v", got, ok)
	}
	links, err := fresh.Links()
	if err != nil {
		t.Fatal(err)
	}
	if links[blob].Token == "" {
		t.Fatal("token missing after one write")
	}
	if got, ok := fresh.SeenBlob(src, fi); !ok || got != blob {
		t.Fatalf("identity index missing after one write: %q ok=%v", got, ok)
	}

	// A repeat of the same record changes nothing, so it must not write at all.
	before = linksWrites.Load()
	if _, err := s.Commit(rec); err != nil {
		t.Fatal(err)
	}
	if n := linksWrites.Load() - before; n != 0 {
		t.Fatalf("an unchanged record wrote %d times", n)
	}
	if inode() != first {
		t.Fatal("an unchanged record rewrote links.json")
	}

	// A record that DOES change something writes exactly once.
	rec.Source = "8:" + src
	before = linksWrites.Load()
	if _, err := s.Commit(rec); err != nil {
		t.Fatal(err)
	}
	if n := linksWrites.Load() - before; n != 1 {
		t.Fatalf("a changed record took %d writes, want exactly 1", n)
	}
	if inode() == first {
		t.Fatal("a changed record did not publish a new file")
	}
}
