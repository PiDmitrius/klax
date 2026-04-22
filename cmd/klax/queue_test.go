package main

import (
	"context"
	"testing"

	"github.com/PiDmitrius/klax/internal/runner"
)

func TestShouldReuseQueuedProgressWithoutGap(t *testing.T) {
	d := newTestDaemon()
	d.chatEvents = map[string]uint64{"tg:1": 3}

	msg := queuedMsg{
		chatID:      "tg:1",
		progressID:  "q1",
		progressSeq: 3,
	}

	if !d.shouldReuseQueuedProgress(msg) {
		t.Fatal("expected queue progress to be reused when chat activity did not move")
	}
}

func TestShouldReuseQueuedProgressReturnsFalseAfterGap(t *testing.T) {
	d := newTestDaemon()
	d.chatEvents = map[string]uint64{"tg:1": 4}

	msg := queuedMsg{
		chatID:      "tg:1",
		progressID:  "q1",
		progressSeq: 3,
	}

	if d.shouldReuseQueuedProgress(msg) {
		t.Fatal("expected queue progress not to be reused after chat activity gap")
	}
}

func TestFormatRunFailureUsesAbortMarkerOnCancel(t *testing.T) {
	got := formatRunFailure([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "🔧 build"},
	}, "", context.Canceled)

	want := "`🔧 build`\n\n❌ Прервано."
	if got != want {
		t.Fatalf("unexpected cancel text:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestSyncFinalMessageChainUsesFreshDeliveryContext(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	chain := newMessageChain("progress-1")
	chain.msgs["progress-1"] = "...\x00html"

	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.syncMessageChain(runCtx, "tg:1", "user-msg", chain, "❌ Прервано.", "html"); err == nil {
		t.Fatal("expected syncMessageChain to fail with canceled run context")
	}

	if _, err := d.syncFinalMessageChain("tg:1", "user-msg", chain, "❌ Прервано.", "html"); err != nil {
		t.Fatalf("syncFinalMessageChain failed: %v", err)
	}
	if tp.editCalls != 1 {
		t.Fatalf("expected one final edit, got %d", tp.editCalls)
	}
	if tp.lastEdit.text != "❌ Прервано." {
		t.Fatalf("unexpected final edit text: %q", tp.lastEdit.text)
	}
}

func TestAbortQueuedMessagesMarksAllQueueProgressAsAborted(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)

	d.abortQueuedMessages([]queuedMsg{
		{chatID: "tg:1", progressID: "q1"},
		{chatID: "tg:1", progressID: "q2"},
		{chatID: "tg:1"},
	})

	if tp.editCalls != 2 {
		t.Fatalf("expected 2 queued progress edits, got %d", tp.editCalls)
	}
	for i, call := range tp.editLog {
		if call.text != "❌ Прервано." {
			t.Fatalf("edit %d text = %q, want %q", i, call.text, "❌ Прервано.")
		}
		if call.message != "q1" && call.message != "q2" {
			t.Fatalf("unexpected message id in edit %d: %q", i, call.message)
		}
	}
}
