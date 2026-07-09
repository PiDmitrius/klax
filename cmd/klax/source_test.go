package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/sealref"
	"github.com/PiDmitrius/klax/internal/session"
)

func newStoreWithChat(sk string, names ...string) *session.Store {
	st := &session.Store{
		Chats: make(map[string]*session.ChatSessions),
		Scope: make(map[string]*session.ScopeDefaults),
	}
	for _, n := range names {
		st.New(sk, n, "/tmp", session.ScopeDefaults{})
	}
	return st
}

// A UI tab can address a stale session; enqueueToSession must reject a missing
// target with a clear error instead of silently falling back to the active one.
func TestEnqueueToSessionRejectsMissingTarget(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.store = newStoreWithChat("tg:1", "one")
	d.runners = make(map[runnerKey]*sessionRunner)

	d.enqueueToSession("tg:1", "100", "hi", nil, 99999, "")

	if len(d.runners) != 0 {
		t.Fatalf("missing target must not create a runner, got %d", len(d.runners))
	}
	var delivered string
	for _, c := range tp.sendLog {
		delivered += c.text
	}
	if !strings.Contains(delivered, "не найдена") {
		t.Fatalf("expected a 'не найдена' rejection, got %q", delivered)
	}
}

// targetCreated > 0 binds to exactly that session even when it is not the active
// one — the property that lets every UI tab run independently. The target runner
// is pre-seeded as processing so the spawned queue pump returns at its guard and
// no real backend runs.
func TestEnqueueToSessionBindsExplicitTarget(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.store = newStoreWithChat("tg:1", "active", "other")
	d.runners = make(map[runnerKey]*sessionRunner)

	var targetCreated, activeCreated int64
	for _, s := range d.store.SessionsFor("tg:1") {
		if s.Active {
			activeCreated = s.Created
		} else {
			targetCreated = s.Created
		}
	}
	if targetCreated == 0 || activeCreated == 0 || targetCreated == activeCreated {
		t.Fatalf("setup: need a distinct active/target pair, got active=%d target=%d", activeCreated, targetCreated)
	}

	sr := &sessionRunner{runner: runner.New(), processing: true}
	d.runners[runnerKey{sk: "tg:1", created: targetCreated}] = sr

	d.enqueueToSession("tg:1", "100", "hi", nil, targetCreated, "")

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.queue) != 1 {
		t.Fatalf("expected 1 queued message on the target runner, got %d", len(sr.queue))
	}
	if sr.queue[0].sessCreated != targetCreated {
		t.Fatalf("message bound to %d, want explicit target %d (not active %d)", sr.queue[0].sessCreated, targetCreated, activeCreated)
	}
}

// A messenger DM (no nonce) is echoed to the UI hub as a "user" event the instant
// it is accepted, so it shows up live in the web UI — not only on a reload.
// A mapped messenger DM lands in the durable log; the read model (the tail's source, not a live
// echo event) renders it as the user's bubble with the original text.
func TestEnqueueToSessionRendersMessengerDMInReadModel(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.identities = map[int64]string{1: "alice"} // tg:1 -> user:alice (a mapped DM)
	d.store = newStoreWithChat("user:alice", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()

	sess := d.store.SessionsFor("user:alice")[0]
	created := sess.Created
	// Pre-seed the runner as processing so the spawned queue pump returns at its
	// guard and no real backend runs.
	d.runners[runnerKey{sk: "user:alice", created: created}] = &sessionRunner{runner: runner.New(), processing: true}

	d.enqueueToSession("tg:1", "100", "hi there", nil, created, "") // mapped messenger DM: no nonce

	found := false
	for _, ut := range d.readModel("user:alice", sess) {
		if ut.Role == "user" && ut.Text == "hi there" {
			found = true
		}
	}
	if !found {
		t.Fatal("messenger DM not rendered as the user's bubble in the read model")
	}
}

// An inbound image the user sends lands in the durable log and the read model (the tail's source,
// not a live echo event) renders it as a markdown image — never the 📎 file fallback.
func TestEnqueueToSessionRendersInboundImageAsMarkdown(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.identities = map[int64]string{1: "alice"} // tg:1 -> user:alice (a mapped DM)
	d.store = newStoreWithChat("user:alice", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()
	var err error
	d.sealer, err = sealref.New()
	if err != nil {
		t.Fatal(err)
	}

	sess := d.store.SessionsFor("user:alice")[0]
	created := sess.Created
	d.runners[runnerKey{sk: "user:alice", created: created}] = &sessionRunner{runner: runner.New(), processing: true}

	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAIAAAB7QOjdAAAAD0lEQVR4nGP8z8DAwMAAAAYIAQHLR3Z1AAAAAElFTkSuQmCC")
	if err != nil {
		t.Fatal(err)
	}
	d.enqueueToSession("tg:1", "100", "", []attachment{{filename: "image.png", data: png}}, created, "")

	found := false
	for _, ut := range d.readModel("user:alice", sess) {
		if ut.Role != "user" {
			continue
		}
		if strings.Contains(ut.Text, "📎") {
			t.Fatalf("image attachment rendered as file fallback: %q", ut.Text)
		}
		if strings.Contains(ut.Text, "![image.png](/api/file?ref=") && strings.Contains(ut.Text, "&w=2&h=1)") {
			found = true
		}
	}
	if !found {
		t.Fatal("inbound image was not rendered as a markdown image in the read model")
	}
}

// Aborting a session marks every still-queued turn err in the durable log; the read model (which
// the tail delivers) then renders both as error turns — no live error event needed.
func TestAbortSessionMarksQueuedTurnsErrInReadModel(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.identities = map[int64]string{1: "alice"} // tg:1 -> user:alice (a mapped DM)
	d.store = newStoreWithChat("user:alice", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()

	sess := d.store.SessionsFor("user:alice")[0]
	created := sess.Created
	d.runners[runnerKey{sk: "user:alice", created: created}] = &sessionRunner{runner: runner.New(), processing: true}
	d.enqueueToSession("tg:1", "100", "one", nil, created, "")
	d.enqueueToSession("tg:1", "101", "two", nil, created, "")

	if !d.abortSession("user:alice", created, false) {
		t.Fatal("abortSession returned false for queued turns")
	}

	errs := 0
	for _, ut := range d.readModel("user:alice", sess) {
		if ut.State == "err" {
			errs++
		}
	}
	if errs != 2 {
		t.Fatalf("aborted queued turns rendered err = %d, want 2", errs)
	}
}
