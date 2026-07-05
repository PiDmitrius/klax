package main

import (
	"errors"
	"os"
	"testing"

	"github.com/PiDmitrius/klax/internal/sessfiles"
)

// removeSessionStore must latch the RUNNER-OWNED store (the instance the in-flight
// run holds), so a late terminal Mark returns ErrRemoved and cannot resurrect the
// dir — a fresh Open() would latch a different object and miss the gap (the F3 bug).
func TestRemoveSessionStoreLatchesRunnerStore(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = newStoreWithChat("tg:1", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	created := d.store.SessionsFor("tg:1")[0].Created
	sr := d.getRunner("tg:1", created)
	seq, _, _, _, err := sr.store.Enqueue("tg:1", "", "n", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.removeSessionStore("tg:1", created) // simulates close/nuke teardown (runner present)
	// The in-flight run's late terminal mark goes through the SAME sr.store instance.
	if err := sr.store.MarkDone(seq); !errors.Is(err, sessfiles.ErrRemoved) {
		t.Fatalf("MarkDone after removeSessionStore = %v, want ErrRemoved", err)
	}
}

// enqueueToSession must durably persist the message (files + enq record) before
// acking, and put a REFERENCE (turnSeq + stored names + marker, not bytes) on the
// in-memory queue. The durable record + file bytes must survive a "restart" (a
// fresh store instance), and an un-run turn must replay as a re-enqueue.
func TestEnqueueToSessionPersistsDurably(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir()) // isolate: this test asserts turnSeq==1

	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.store = newStoreWithChat("tg:1", "one")
	d.runners = make(map[runnerKey]*sessionRunner)

	created := d.store.SessionsFor("tg:1")[0].Created
	sr := d.getRunner("tg:1", created)
	sr.processing = true // busy → enqueue only queues; no real backend runs

	att := []attachment{{filename: "shot.png", data: []byte("PNG")}}
	if ok := d.enqueueToSession("tg:1", "100", "look", att, created, ""); !ok {
		t.Fatal("enqueueToSession returned false")
	}

	// The in-memory queue holds a reference, not the bytes.
	sr.mu.Lock()
	n := len(sr.queue)
	var qm queuedMsg
	if n == 1 {
		qm = sr.queue[0]
	}
	sr.mu.Unlock()
	if n != 1 {
		t.Fatalf("queue len = %d, want 1", n)
	}
	if qm.turnSeq != 1 || qm.marker == "" || len(qm.files) != 1 {
		t.Fatalf("queued ref wrong: turnSeq=%d marker=%q files=%v", qm.turnSeq, qm.marker, qm.files)
	}

	// Durable: a fresh store (restart) sees the enq with text + file + marker, and
	// the file bytes are on disk.
	fresh := sessfiles.Open("tg:1", created)
	log, err := fresh.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "look" || len(log[0].Files) != 1 || log[0].Marker != qm.marker {
		t.Fatalf("durable inbound log mismatch: %+v", log)
	}
	if b, _ := os.ReadFile(fresh.Path(log[0].Files[0])); string(b) != "PNG" {
		t.Fatalf("stored file bytes = %q, want PNG", b)
	}

	// Un-run → replay re-enqueues it (not recover).
	reenq, recovered, err := fresh.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(reenq) != 1 || len(recovered) != 0 {
		t.Fatalf("replay: reenq=%d recovered=%d, want 1/0", len(reenq), len(recovered))
	}
}

func TestReplayDurableQueuesLeavesRecoveredRunUnchanged(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	d := newTestDeliveryDaemon(&fakeTransport{})
	d.store = newStoreWithChat("tg:1", "one")
	d.runners = make(map[runnerKey]*sessionRunner)

	created := d.store.SessionsFor("tg:1")[0].Created
	sr := d.getRunner("tg:1", created)
	seq, _, _, _, err := sr.store.Enqueue("tg:1", "", "n", "already ran", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sr.store.MarkRun(seq); err != nil {
		t.Fatal(err)
	}

	d.replayDurableQueues()

	log, err := sessfiles.Open("tg:1", created).InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Last != "run" || log[0].Reason != "" {
		t.Fatalf("recovered run must stay run for transcript/readmodel reconciliation: %+v", log)
	}
}

func TestEnqueueToSessionDuplicateNonceDoesNotRequeue(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	d.store = newStoreWithChat("user:claw", "one")
	d.runners = make(map[runnerKey]*sessionRunner)
	d.uiHub = newUIHub()

	created := d.store.SessionsFor("user:claw")[0].Created
	sr := d.getRunner("user:claw", created)
	sr.processing = true

	if ok := d.enqueueToSession("user:claw", "100", "one", nil, created, "nonce-1"); !ok {
		t.Fatal("first enqueueToSession returned false")
	}
	if ok := d.enqueueToSession("user:claw", "101", "retry", nil, created, "nonce-1"); !ok {
		t.Fatal("duplicate enqueueToSession returned false")
	}

	// The duplicate is dropped: it neither requeues nor writes a second durable turn. Because the
	// UI now renders the durable inbound log (no separate echo event), one log entry == one user
	// bubble, so this also proves the duplicate never double-surfaces to the tab.
	sr.mu.Lock()
	n := len(sr.queue)
	sr.mu.Unlock()
	if n != 1 {
		t.Fatalf("queue len after duplicate nonce = %d, want 1", n)
	}
	log, err := sr.store.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "one" {
		t.Fatalf("inbound log after duplicate nonce = %+v, want one original turn", log)
	}
}
