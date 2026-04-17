package main

import "testing"

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
