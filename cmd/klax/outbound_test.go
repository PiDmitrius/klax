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
	out := d.rewriteOutboundForUI("tg:1", created, md)

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
	if got := d.rewriteOutboundForUI("tg:1", created, md); got != md {
		t.Fatalf("UI-off must pass through unchanged: %q", got)
	}
}
