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

// A queued-progress placeholder is created (in enqueueToSession) as a reply to
// the user's message; when nothing else happened in the chat, newMessengerDelivery
// reuses it directly. Its edits (both the "..." placeholder and the final
// answer) must resend that original replyTo — ym drops the reply link
// otherwise, and it was silently missing until replyTos was seeded here too.
func TestMessengerDeliveryReusedQueuedProgressKeepsReplyToOnEdit(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	seq := d.bumpChatActivity("tg:1")
	msg := queuedMsg{chatID: "tg:1", msgID: "100", progressID: "queued-1", progressSeq: seq, sessKey: "user:x", sessCreated: 1}

	del := d.newMessengerDelivery(context.Background(), msg, true)
	del.Final(runner.RunResult{Text: "pong"})

	if tp.editCalls == 0 {
		t.Fatalf("expected the answer to be delivered via an edit, got 0 edits")
	}
	for _, call := range tp.editLog {
		if call.message != "queued-1" {
			continue
		}
		if call.replyTo != "100" {
			t.Errorf("edit of reused queued progress %q lost replyTo, got %q", call.message, call.replyTo)
		}
	}
}

// If chat activity moved on before the run started, the queued placeholder is
// NOT reused as the progress message — it's redirected to a "↓" marker instead
// (pointing at the new answer below). That redirect is itself an edit of a
// message that was created as a reply, so it must resend replyTo too.
func TestMessengerDeliveryRedirectMarkerKeepsReplyTo(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	seq := d.bumpChatActivity("tg:1")
	d.bumpChatActivity("tg:1") // more activity since queueing -> reuse condition fails
	msg := queuedMsg{chatID: "tg:1", msgID: "100", progressID: "queued-1", progressSeq: seq, sessKey: "user:x", sessCreated: 1}

	d.newMessengerDelivery(context.Background(), msg, true)

	var found bool
	for _, call := range tp.editLog {
		if call.text != "↓" {
			continue
		}
		found = true
		if call.replyTo != "100" {
			t.Errorf("redirect marker edit lost replyTo, got %q", call.replyTo)
		}
	}
	if !found {
		t.Fatal("expected a redirect-marker (\"↓\") edit for the unused queued placeholder")
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
