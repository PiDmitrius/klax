package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/runner"
)

func TestShouldReuseQueuedProgressWithoutGap(t *testing.T) {
	d := newTestDaemon(t)
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
	d := newTestDaemon(t)
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
	chunks := formatRunFailureChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "🔧 build"},
	}, "", context.Canceled)
	got := strings.Join(chunks, "\n\n")

	want := "`🔧 build`\n\n❌ Прервано."
	if got != want {
		t.Fatalf("unexpected cancel text:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestFormatRunFailureIsTerminal(t *testing.T) {
	chunks := formatRunFailureChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "⚙️ Exec: `true`"},
		{Kind: runner.ProgressKindTool, Text: "❓ error"},
	}, "", errors.New("codex exited"))
	got := strings.Join(chunks, "\n\n")

	want := "`⚙️ Exec: 'true'`\n`❓ error`\n\n❌ Ошибка: codex exited"
	if got != want {
		t.Fatalf("unexpected failure text:\nwant: %q\ngot:  %q", want, got)
	}
	if strings.Contains(got, "...") {
		t.Fatalf("terminal failure retains a working marker: %q", got)
	}
}

func TestFormatRunFailureEscapesErrorTextForMarkup(t *testing.T) {
	err := errors.New("model <x> is at capacity & overloaded")
	for _, format := range []string{"html", "rich"} {
		got := strings.Join(formatRunFailureChunks(nil, format, err), "\n\n")
		if strings.Contains(got, "<x>") || strings.Contains(got, " & ") {
			t.Fatalf("%s failure text keeps raw markup: %q", format, got)
		}
		if !strings.Contains(got, "&lt;x&gt;") || !strings.Contains(got, "&amp;") {
			t.Fatalf("%s failure text is not escaped: %q", format, got)
		}
	}
}

func TestFormatRunFailureReplacesWholeProgressChain(t *testing.T) {
	logItems := []runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "⚙️ Exec: `first`"},
		{Kind: runner.ProgressKindNarration, Text: strings.Repeat("narration ", 350)},
		{Kind: runner.ProgressKindTool, Text: "⚙️ Exec: `second`"},
		{Kind: runner.ProgressKindTool, Text: "⚙️ Exec: `third`"},
	}
	progress := withProgressEllipsis(formatLogChunks(logItems, "", "", maxMessageLen), "", maxMessageLen)
	final := formatRunFailureChunks(logItems, "", errors.New("terminal"))
	if len(final) < len(progress) {
		t.Fatalf("final chain has %d chunks, progress chain has %d; stale working messages would survive", len(final), len(progress))
	}
	if strings.HasSuffix(final[len(final)-1], "...") {
		t.Fatalf("terminal chain retains working marker: %q", final[len(final)-1])
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
	chunks := withProgressEllipsis([]string{"progress"}, "", maxMessageLen)
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %#v", chunks)
	}
	if chunks[0] != "progress\n\n..." {
		t.Fatalf("unexpected chunk: %q", chunks[0])
	}
}

func TestWithProgressEllipsisAppendsToFullChunk(t *testing.T) {
	chunks := withProgressEllipsis([]string{strings.Repeat("x", 30)}, "", maxMessageLen)
	if len(chunks) != 1 {
		t.Fatalf("expected ellipsis on existing chunk, got %#v", chunks)
	}
	if !strings.HasSuffix(chunks[0], "\n\n...") {
		t.Fatalf("expected ellipsis on existing chunk, got %#v", chunks)
	}
}

func TestProgressLogChunksAlwaysEndWithEllipsis(t *testing.T) {
	chunks := withProgressEllipsis(formatLogChunks([]runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("a", 20)},
		{Kind: runner.ProgressKindTool, Text: "🔧 " + strings.Repeat("b", 20)},
	}, "", "html", 46), "", maxMessageLen)
	if len(chunks) != 2 {
		t.Fatalf("expected split progress chunks, got %#v", chunks)
	}
	if !strings.HasSuffix(chunks[len(chunks)-1], "\n\n...") {
		t.Fatalf("expected progress ellipsis on final chunk, got %#v", chunks)
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
		{chatID: "tg:1", msgID: "user-1", progressID: "q1"},
		{chatID: "tg:1", msgID: "user-2", progressID: "q2"},
		{chatID: "tg:1"},
	})

	if tp.editCalls != 2 {
		t.Fatalf("expected 2 queued progress edits, got %d", tp.editCalls)
	}
	wantReply := map[string]string{"q1": "user-1", "q2": "user-2"}
	for i, call := range tp.editLog {
		if call.text != "❌ Прервано." {
			t.Fatalf("edit %d text = %q, want %q", i, call.text, "❌ Прервано.")
		}
		want, ok := wantReply[call.message]
		if !ok {
			t.Fatalf("unexpected message id in edit %d: %q", i, call.message)
		}
		// The placeholder was created as a reply to its own message — the
		// abort edit must resend that same replyTo (ym drops it otherwise).
		if call.replyTo != want {
			t.Fatalf("edit %d (message %q) replyTo = %q, want %q", i, call.message, call.replyTo, want)
		}
	}
}

func TestNotifyQueuePositionsUpdatesPositionAndReplyTo(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)

	d.notifyQueuePositions([]queuedMsg{
		{chatID: "tg:1", msgID: "user-1", progressID: "q1"},
		{chatID: "tg:1", msgID: "user-2", progressID: "q2"},
		{chatID: "tg:1"}, // no progressID — nothing to edit for this one
	})

	if tp.editCalls != 2 {
		t.Fatalf("expected 2 position edits, got %d", tp.editCalls)
	}
	wantPos := map[string]string{"q1": "⏳ В очереди: 1", "q2": "⏳ В очереди: 2"}
	wantReply := map[string]string{"q1": "user-1", "q2": "user-2"}
	for i, call := range tp.editLog {
		want, ok := wantPos[call.message]
		if !ok {
			t.Fatalf("unexpected message id in edit %d: %q", i, call.message)
		}
		if call.text != want {
			t.Fatalf("edit %d (message %q) text = %q, want %q", i, call.message, call.text, want)
		}
		// The placeholder was created as a reply to its own message — the
		// position-update edit must resend that same replyTo.
		if call.replyTo != wantReply[call.message] {
			t.Fatalf("edit %d (message %q) replyTo = %q, want %q", i, call.message, call.replyTo, wantReply[call.message])
		}
	}
}
