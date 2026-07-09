package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/sealref"
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
	sealer, err := sealref.New()
	if err != nil {
		t.Fatal(err)
	}
	d.sealer = sealer
	ref, err := d.mintFileRef("user:alice", sess.Created, store.Path(stored), "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/file?ref="+ref, nil)
	rec := httptest.NewRecorder()
	(&uiServer{d: d}).handleFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleFile code = %d, want 200", rec.Code)
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
	d.sealer, err = sealref.New()
	if err != nil {
		t.Fatal(err)
	}

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
