package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/sessfiles"
	"github.com/PiDmitrius/klax/internal/session"
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

func TestHandleFileUsesDisplayNameForDownload(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	cwd := t.TempDir()
	src := filepath.Join(cwd, "report.md")
	if err := os.WriteFile(src, []byte("plan"), 0600); err != nil {
		t.Fatal(err)
	}

	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
	sess := d.store.New("user:alice", "one", cwd, session.ScopeDefaults{})
	store := sessfiles.Open("user:alice", sess.Created)
	stored, err := store.Adopt(filepath.Base(src), src)
	if err != nil {
		t.Fatal(err)
	}
	token, err := d.fileToken(store, "user:alice", sess.Created, stored, sessfiles.DisplayName(stored), "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/file?ref="+token, nil)
	rec := httptest.NewRecorder()
	(&uiServer{d: d}).handleFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleFile code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != fileCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, fileCacheControl)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, `filename="report.md"`) {
		t.Fatalf("Content-Disposition = %q, want display filename", cd)
	}
	if strings.Contains(cd, "out-") {
		t.Fatalf("Content-Disposition leaked durable name: %q", cd)
	}
}

func TestInboundTextShowsAttachmentSize(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	var err error

	store := sessfiles.Open("user:alice", 1)
	_, _, _, _, err = store.Enqueue("user:alice", "", "nonce", "see attached", []sessfiles.NamedReader{{
		Name: "report.md",
		R:    bytes.NewReader(bytes.Repeat([]byte("x"), 10000)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 {
		t.Fatalf("log len = %d, want 1", len(log))
	}

	got := d.inboundText(store, log[0], "user:alice", 1)
	if !strings.Contains(got, "[report.md](/api/file?ref=") {
		t.Fatalf("inboundText missing attachment link: %q", got)
	}
	if !strings.Contains(got, ") (9.8 KiB)") {
		t.Fatalf("inboundText missing human size: %q", got)
	}
}

// sessionStore must return ONE canonical Store per (sk,created), and after removeSessionStore no late
// call (a different sessfiles.Open would have its own clean latch) may resurrect the session dir.
func TestSessionStoreCanonicalNoResurrection(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	sk, created := "user:alice", int64(1)

	if s1, s2 := d.sessionStore(sk, created), d.sessionStore(sk, created); s1 != s2 {
		t.Fatal("sessionStore must return one canonical instance")
	}
	if _, err := d.sessionStore(sk, created).EnsureLink("000001-01-a.png", "a.png", "image/png"); err != nil {
		t.Fatal(err)
	}
	dir := sessfiles.WorkDir(sk, created)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir should exist after EnsureLink: %v", err)
	}

	d.removeSessionStore(sk, created)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be gone after removeSessionStore: %v", err)
	}
	// A late EnsureLink through the canonical store must refuse and NOT re-create the directory.
	if _, err := d.sessionStore(sk, created).EnsureLink("000001-01-b.png", "b.png", "image/png"); err != sessfiles.ErrRemoved {
		t.Fatalf("late EnsureLink after remove = %v, want ErrRemoved", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("late EnsureLink must not resurrect the dir: stat=%v", err)
	}
}

// After delete, a new session created the same second must get a DIFFERENT Created (never reused) and
// therefore a FRESH, writable canonical Store — not the deleted session's removed one.
func TestNewSessionAfterDeleteGetsFreshStore(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
	sk := "user:alice"

	a := d.store.New(sk, "a", "/tmp", session.ScopeDefaults{})
	if _, err := d.sessionStore(sk, a.Created).EnsureLink("000001-01-x.png", "x.png", "image/png"); err != nil {
		t.Fatal(err)
	}
	d.removeSessionStore(sk, a.Created) // marks a's canonical Store removed
	if !d.store.Delete(sk, 0) {
		t.Fatal("delete failed")
	}

	b := d.store.New(sk, "b", "/tmp", session.ScopeDefaults{})
	if b.Created == a.Created {
		t.Fatalf("Created reused after delete: %d", b.Created)
	}
	// The new session's canonical Store must be a fresh, writable one (not a's removed Store).
	if _, err := d.sessionStore(sk, b.Created).EnsureLink("000001-01-y.png", "y.png", "image/png"); err != nil {
		t.Fatalf("new session's store must be writable, got %v", err)
	}
}

// A file token must be STABLE across read-model rebuilds and persist in links.json across a reopen
// (restart) — otherwise the attachment's <img src> changes and the image re-decodes/flickers.
func TestFileTokenStableAndPersisted(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	store := sessfiles.Open("user:alice", 1)
	t1, err := d.fileToken(store, "user:alice", 1, "000001-01-a.png", "a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := d.fileToken(store, "user:alice", 1, "000001-01-a.png", "a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 {
		t.Fatalf("token not stable across rebuilds: %q vs %q", t1, t2)
	}
	// A different file gets a distinct token.
	t3, err := d.fileToken(store, "user:alice", 1, "000001-01-b.png", "b.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if t3 == t1 {
		t.Fatal("distinct files must get distinct tokens")
	}
	// The token survives a fresh Store.Open (i.e. a restart) — it is durable in links.json.
	links, err := sessfiles.Open("user:alice", 1).Links()
	if err != nil {
		t.Fatal(err)
	}
	if links["000001-01-a.png"].Token != t1 {
		t.Fatalf("token must persist in links.json across reopen: %q", links["000001-01-a.png"].Token)
	}
}
