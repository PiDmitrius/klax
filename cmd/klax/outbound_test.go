package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/sessfiles"
	"github.com/PiDmitrius/klax/internal/session"
)

// rewriteOutboundForUI: an in-root file link/image becomes a capability URL (and is
// snapshotted into the durable store); an out-of-root link degrades to plain text
// (never a dead local path); a remote link is untouched.
func TestRewriteOutboundForUI(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	cwd := t.TempDir()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAIAAAB7QOjdAAAAD0lEQVR4nGP8z8DAwMAAAAYIAQHLR3Z1AAAAAElFTkSuQmCC")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "chart.png"), png, 0600); err != nil {
		t.Fatal(err)
	}

	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
	d.store.New("tg:1", "one", cwd, session.ScopeDefaults{})
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub() // UI on: the file-link rewrite is enabled
	created := d.store.SessionsFor("tg:1")[0].Created

	md := "img ![c](chart.png) esc [r](../../../etc/passwd) web [w](https://x.com/a)"
	out := d.rewriteOutboundForUI("tg:1", created, 1, md)

	if !strings.Contains(out, "![c](/api/file?ref=") {
		t.Fatalf("in-root image must be rewritten to a capability URL: %q", out)
	}
	if !strings.Contains(out, "&w=2&h=1)") {
		t.Fatalf("rewritten local image must carry dimensions: %q", out)
	}
	if strings.Contains(out, "passwd") {
		t.Fatalf("out-of-root link must degrade to its label: %q", out)
	}
	if !strings.Contains(out, "[w](https://x.com/a)") {
		t.Fatalf("remote link must be untouched: %q", out)
	}
	// The snapshot landed in the durable store as an out-* entry.
	filesDir := filepath.Dir(sessfiles.Open("tg:1", created).Path("x"))
	ents, _ := os.ReadDir(filesDir)
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "out-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("outbound file was not snapshotted into %s", filesDir)
	}

	// With the UI off the markdown is returned unchanged.
	d.uiHub = nil
	if got := d.rewriteOutboundForUI("tg:1", created, 1, md); got != md {
		t.Fatalf("UI-off must pass through unchanged: %q", got)
	}
}

// A link keeps rendering after the agent deletes, renames or moves the original.
func TestRewriteOutboundSurvivesADeletedOriginal(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	cwd := t.TempDir()
	src := filepath.Join(cwd, "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
	d.store.New("tg:1", "one", cwd, session.ScopeDefaults{})
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()
	created := d.store.SessionsFor("tg:1")[0].Created

	md := "see [report](report.csv)"
	first := d.rewriteOutboundForUI("tg:1", created, 1, md)
	if !strings.Contains(first, "[report](/api/file?ref=") {
		t.Fatalf("first render must publish the file: %q", first)
	}

	if err := os.Rename(src, filepath.Join(t.TempDir(), "moved.csv")); err != nil {
		t.Fatal(err)
	}

	second := d.rewriteOutboundForUI("tg:1", created, 1, md)
	if second != first {
		t.Fatalf("a rebuild after the original vanished changed the link:\n first=%q\nsecond=%q", first, second)
	}
	if !strings.Contains(second, "/api/file?ref=") {
		t.Fatalf("link degraded to plain text although the snapshot is in the store: %q", second)
	}
}

// A later turn linking the same path serves that turn's content; the earlier turn keeps its own.
func TestRewriteOutboundCapturesEachTurnsVersion(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	cwd := t.TempDir()
	src := filepath.Join(cwd, "summary.md")
	if err := os.WriteFile(src, []byte("# v1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
	d.store.New("tg:1", "one", cwd, session.ScopeDefaults{})
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()
	created := d.store.SessionsFor("tg:1")[0].Created

	md := "готово [summary](summary.md)"
	turn1 := d.rewriteOutboundForUI("tg:1", created, 1, md)

	if err := os.WriteFile(src, []byte("# v2 corrected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn2 := d.rewriteOutboundForUI("tg:1", created, 2, md)

	if turn2 == turn1 {
		t.Fatalf("the corrected file was served as the old snapshot — the fix is invisible: %q", turn2)
	}
	if again := d.rewriteOutboundForUI("tg:1", created, 1, md); again != turn1 {
		t.Fatalf("re-rendering turn 1 changed its link:\n was=%q\nnow=%q", turn1, again)
	}
	body1, body2 := servedBody(t, d, turn1), servedBody(t, d, turn2)
	if body1 != "# v1\n" {
		t.Fatalf("turn 1 serves %q, want the version it delivered", body1)
	}
	if body2 != "# v2 corrected\n" {
		t.Fatalf("turn 2 serves %q, want the corrected version", body2)
	}
}

// servedBody resolves a rewritten link back to the bytes it serves.
func servedBody(t *testing.T, d *daemon, rewritten string) string {
	t.Helper()
	i := strings.Index(rewritten, "ref=")
	if i < 0 {
		t.Fatalf("no capability ref in %q", rewritten)
	}
	tok := rewritten[i+len("ref="):]
	if j := strings.IndexAny(tok, "&)"); j >= 0 {
		tok = tok[:j]
	}
	d.fileTokensMu.Lock()
	ref, ok := d.fileTokens[tok]
	d.fileTokensMu.Unlock()
	if !ok {
		t.Fatalf("token %q not in the index", tok)
	}
	data, err := os.ReadFile(sessfiles.Open(ref.sk, ref.created).Path(ref.stored))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A fresh Store (a restart) resolves the same link.
func TestRewriteOutboundResolvesAfterRestart(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	cwd := t.TempDir()
	src := filepath.Join(cwd, "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	newDaemon := func() (*daemon, int64) {
		d := newTestDeliveryDaemon(&fakeTransport{})
		d.store = &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}}
		d.store.New("tg:1", "one", cwd, session.ScopeDefaults{})
		d.runners = make(map[runnerKey]*sessionRunner)
		d.uiHub = newUIHub()
		return d, d.store.SessionsFor("tg:1")[0].Created
	}

	md := "see [report](report.csv)"
	d1, created1 := newDaemon()
	first := d1.rewriteOutboundForUI("tg:1", created1, 1, md)
	if !strings.Contains(first, "/api/file?ref=") {
		t.Fatalf("first render must publish the file: %q", first)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}

	d2, created2 := newDaemon()
	if created2 != created1 {
		t.Fatalf("test setup did not reproduce the same durable store (%d vs %d)", created1, created2)
	}
	second := d2.rewriteOutboundForUI("tg:1", created2, 1, md)
	if second != first {
		t.Fatalf("after restart the link changed:\n first=%q\nsecond=%q", first, second)
	}
}
