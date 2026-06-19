package main

import (
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/runner"
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
func TestEnqueueToSessionEchoesUserToUI(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.identities = map[int64]string{1: "claw"} // tg:1 -> user:claw (a mapped DM)
	d.store = newStoreWithChat("user:claw", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()

	created := d.store.SessionsFor("user:claw")[0].Created
	// Pre-seed the runner as processing so the spawned queue pump returns at its
	// guard and no real backend runs.
	d.runners[runnerKey{sk: "user:claw", created: created}] = &sessionRunner{runner: runner.New(), processing: true}

	d.enqueueToSession("tg:1", "100", "hi there", nil, created, "") // mapped messenger DM: no nonce

	ev, _, _ := d.uiHub.collect("claw", d.uiHub.epoch, 0)
	found := false
	for _, raw := range ev {
		if e := decodeEvent(t, raw); e.Type == "user" && e.Text == "hi there" && e.Session == created {
			found = true
		}
	}
	if !found {
		t.Fatalf("messenger DM not echoed to the UI hub as a user event; got %d events", len(ev))
	}
}
