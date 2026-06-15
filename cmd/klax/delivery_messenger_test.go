package main

import (
	"context"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/runner"
)

// Progress runs in the runner's stdout goroutine under a lock and must never
// block on a full mailbox; it must also keep the snapshot cumulative so a
// dropped stale snapshot loses no history.
func TestMessengerDeliveryProgressNonBlockingCumulative(t *testing.T) {
	m := &messengerDelivery{
		verbose:      true,
		progressCh:   make(chan []runner.ProgressEvent, 1),
		progressDone: make(chan struct{}),
		workerDone:   make(chan struct{}),
	}
	// No worker is draining progressCh (buffer of 1). Five Progress calls must
	// all return; if Progress blocked, the test would deadlock here.
	for i := 0; i < 5; i++ {
		m.Progress(runner.ProgressEvent{Kind: runner.ProgressKindTool, Text: "ev"})
	}
	snap := <-m.progressCh
	if len(snap) != 5 {
		t.Fatalf("expected cumulative snapshot of 5 events, got %d", len(snap))
	}
	if len(m.logItems) != 5 {
		t.Fatalf("expected 5 accumulated logItems, got %d", len(m.logItems))
	}
}

// A non-verbose delivery drops progress entirely (parity with the old
// onProgress !verbose early return).
func TestMessengerDeliveryProgressDroppedWhenQuiet(t *testing.T) {
	m := &messengerDelivery{
		verbose:      false,
		progressCh:   make(chan []runner.ProgressEvent, 1),
		progressDone: make(chan struct{}),
		workerDone:   make(chan struct{}),
	}
	m.Progress(runner.ProgressEvent{Kind: runner.ProgressKindTool, Text: "ev"})
	if len(m.logItems) != 0 {
		t.Fatalf("quiet delivery must not accumulate, got %d", len(m.logItems))
	}
	select {
	case <-m.progressCh:
		t.Fatal("quiet delivery must not enqueue a snapshot")
	default:
	}
}

// End-to-end wiring: newMessengerDelivery creates the placeholder via the
// transport, and Final edits that chain with the rendered answer.
func TestMessengerDeliveryDeliversAnswer(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	msg := queuedMsg{chatID: "tg:1", msgID: "100", sessKey: "user:x", sessCreated: 1}

	del := d.newMessengerDelivery(context.Background(), msg, true)
	del.Final(runner.RunResult{Text: "hello world"})

	if tp.editCalls == 0 {
		t.Fatalf("expected the answer to be delivered via an edit, got 0 edits")
	}
	if !strings.Contains(tp.lastEdit.text, "hello world") {
		t.Fatalf("final edit missing answer: %q", tp.lastEdit.text)
	}
}

// Final must use a fresh delivery context, not the run context: /abort cancels
// the run ctx, yet the "❌ Прервано." result still has to reach the user.
func TestMessengerDeliveryFinalSurvivesCancelledRunCtx(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	msg := queuedMsg{chatID: "tg:1", msgID: "100", sessKey: "user:x", sessCreated: 1}

	ctx, cancel := context.WithCancel(context.Background())
	del := d.newMessengerDelivery(ctx, msg, true)
	cancel() // simulate /abort after the placeholder exists

	del.Final(runner.RunResult{Error: context.Canceled})

	if tp.editCalls == 0 && tp.sendCalls == 0 {
		t.Fatal("aborted-run final was not delivered at all")
	}
	delivered := tp.lastEdit.text
	for _, c := range tp.sendLog {
		delivered += c.text
	}
	if !strings.Contains(delivered, "Прервано") {
		t.Fatalf("expected abort notice in final delivery, got %q", delivered)
	}
}
