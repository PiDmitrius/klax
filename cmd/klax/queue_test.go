package main

import (
	"context"
	"strings"
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

func TestFormatLogChunksKeepsToolEntriesAtomic(t *testing.T) {
	chunks := formatLogChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("a", 20)},
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("b", 20)},
	}, "...", "", 32)

	if len(chunks) != 2 {
		t.Fatalf("expected one chunk per tool entry, got %d: %#v", len(chunks), chunks)
	}
	if strings.Contains(chunks[0], "b") || strings.Contains(chunks[1], "a") {
		t.Fatalf("tool entries were mixed across chunks: %#v", chunks)
	}
	if !strings.HasSuffix(chunks[1], "...") {
		t.Fatalf("expected tail on final chunk, got %#v", chunks)
	}
}

func TestFormatLogChunksKeepsHTMLToolEntriesAtomic(t *testing.T) {
	chunks := formatLogChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("a", 20)},
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("b", 20)},
	}, "...", "html", 46)

	if len(chunks) != 2 {
		t.Fatalf("expected one chunk per tool entry, got %d: %#v", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if err := validateHTMLNesting(chunk); err != nil {
			t.Fatalf("chunk %d invalid html nesting: %v\n%s", i, err, chunk)
		}
	}
	if strings.Contains(chunks[0], "b") || strings.Contains(chunks[1], "a") {
		t.Fatalf("tool entries were mixed across chunks: %#v", chunks)
	}
	if !strings.HasSuffix(chunks[1], "...") {
		t.Fatalf("expected tail on final chunk, got %#v", chunks)
	}
}

func TestFormatLogChunksSplitsOversizedHTMLSegmentSafely(t *testing.T) {
	text := "🔧 " + strings.Repeat("tool ", 30)
	chunks := formatLogChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: text},
	}, "", "html", 64)

	if len(chunks) < 2 {
		t.Fatalf("expected oversized tool entry to split, got %#v", chunks)
	}
	var rebuilt strings.Builder
	for i, chunk := range chunks {
		if len(chunk) > 64 {
			t.Fatalf("chunk %d too large: %d", i, len(chunk))
		}
		if err := validateHTMLNesting(chunk); err != nil {
			t.Fatalf("chunk %d invalid html nesting: %v\n%s", i, err, chunk)
		}
		rebuilt.WriteString(stripHTML(chunk))
	}
	if rebuilt.String() != text {
		t.Fatalf("visible text mismatch after oversized split")
	}
}

func TestFormatLogChunksSplitsOversizedHTMLNarrationSafely(t *testing.T) {
	text := strings.Repeat("**важно** проверить список\n\n", 8)
	chunks := formatLogChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindNarration, Text: text},
	}, "", "html", 72)

	if len(chunks) < 2 {
		t.Fatalf("expected oversized narration to split, got %#v", chunks)
	}
	for i, chunk := range chunks {
		if len(chunk) > 72 {
			t.Fatalf("chunk %d too large: %d", i, len(chunk))
		}
		if err := validateHTMLNesting(chunk); err != nil {
			t.Fatalf("chunk %d invalid html nesting: %v\n%s", i, err, chunk)
		}
	}
}

func TestWithProgressEllipsisAppendsWhenItFits(t *testing.T) {
	chunks := withProgressEllipsis([]string{"progress"})
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %#v", chunks)
	}
	if chunks[0] != "progress\n\n..." {
		t.Fatalf("unexpected chunk: %q", chunks[0])
	}
}

func TestWithProgressEllipsisAppendsToFullChunk(t *testing.T) {
	chunks := withProgressEllipsis([]string{strings.Repeat("x", 30)})
	if len(chunks) != 1 {
		t.Fatalf("expected ellipsis on existing chunk, got %#v", chunks)
	}
	if !strings.HasSuffix(chunks[0], "\n\n...") {
		t.Fatalf("expected ellipsis on existing chunk, got %#v", chunks)
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

func TestSyncFinalMessageChainChunksUsesFreshDeliveryContext(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	chain := newMessageChain("progress-1")
	chain.msgs["progress-1"] = "...\x00html"

	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.syncMessageChainChunks(runCtx, "tg:1", "user-msg", chain, []string{"❌ Прервано."}, "html"); err == nil {
		t.Fatal("expected syncMessageChainChunks to fail with canceled run context")
	}

	if _, err := d.syncFinalMessageChainChunks("tg:1", "user-msg", chain, []string{"❌ Прервано."}, "html"); err != nil {
		t.Fatalf("syncFinalMessageChainChunks failed: %v", err)
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
