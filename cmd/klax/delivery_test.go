package main

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PiDmitrius/klax/internal/transport"
)

func TestSplitMessageHTMLKeepsTagsBalanced(t *testing.T) {
	text := strings.Repeat(`<b>bold text</b> <i>italic text</i> <a href="https://example.com">link</a>`+"\n", 12)
	chunks := splitMessage(text, 120, "html")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > 120 {
			t.Fatalf("chunk %d too large: %d", i, len(chunk))
		}
		if err := validateHTMLNesting(chunk); err != nil {
			t.Fatalf("chunk %d invalid html nesting: %v\n%s", i, err, chunk)
		}
	}
}

func TestSplitMessageHTMLPreservesVisibleText(t *testing.T) {
	text := "<b>" + strings.Repeat("hello world ", 30) + "</b>"
	chunks := splitMessage(text, 64, "html")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	var rebuilt strings.Builder
	for _, chunk := range chunks {
		rebuilt.WriteString(stripHTML(chunk))
	}
	if rebuilt.String() != stripHTML(text) {
		t.Fatalf("visible text mismatch after split")
	}
}

func TestSplitMessagePreservesUTF8Runes(t *testing.T) {
	text := strings.Repeat("обновлён ", 40)
	chunks := splitMessage(text, 17, "")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	var rebuilt strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid utf-8: %q", i, chunk)
		}
		rebuilt.WriteString(chunk)
	}
	if rebuilt.String() != text {
		t.Fatalf("text mismatch after utf-8 split")
	}
}

func TestSplitMessageHTMLPreservesUTF8Runes(t *testing.T) {
	text := "<b>" + strings.Repeat("обновлён ", 30) + "</b>"
	chunks := splitMessage(text, 23, "html")
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	var rebuilt strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid utf-8: %q", i, chunk)
		}
		if err := validateHTMLNesting(chunk); err != nil {
			t.Fatalf("chunk %d invalid html nesting: %v\n%s", i, err, chunk)
		}
		rebuilt.WriteString(stripHTML(chunk))
	}
	if rebuilt.String() != stripHTML(text) {
		t.Fatalf("visible text mismatch after html utf-8 split")
	}
}

func validateHTMLNesting(text string) error {
	var stack []string
	for i := 0; i < len(text); {
		if text[i] != '<' {
			i++
			continue
		}
		end := strings.IndexByte(text[i:], '>')
		if end == -1 {
			return &testError{"unterminated tag"}
		}
		tag := text[i : i+end+1]
		name, closing, selfClosing, ok := parseHTMLTag(tag)
		if !ok {
			i += end + 1
			continue
		}
		if selfClosing {
			i += end + 1
			continue
		}
		if closing {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return &testError{"unexpected closing tag"}
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
		i += end + 1
	}
	if len(stack) != 0 {
		return &testError{"unclosed tags"}
	}
	return nil
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

type fakeSendCall struct {
	chatID  string
	text    string
	replyTo string
	format  string
}

type fakeEditCall struct {
	chatID  string
	message string
	text    string
	format  string
}

type fakeTransport struct {
	sendIDs   []string
	sendCalls int
	editCalls int
	editErr   error
	sendErrFn func(call fakeSendCall) error
	sendLog   []fakeSendCall
	editLog   []fakeEditCall
	lastEdit  struct {
		chatID  string
		message string
		text    string
		format  string
	}
}

func (f *fakeTransport) SendMessage(chatID, text, replyTo, format string) error {
	call := fakeSendCall{chatID: chatID, text: text, replyTo: replyTo, format: format}
	f.sendCalls++
	f.sendLog = append(f.sendLog, call)
	if f.sendErrFn != nil {
		return f.sendErrFn(call)
	}
	return nil
}

func (f *fakeTransport) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	call := fakeSendCall{chatID: chatID, text: text, replyTo: replyTo, format: format}
	f.sendCalls++
	f.sendLog = append(f.sendLog, call)
	if f.sendErrFn != nil {
		if err := f.sendErrFn(call); err != nil {
			return "", err
		}
	}
	if len(f.sendIDs) == 0 {
		return "generated-id", nil
	}
	id := f.sendIDs[0]
	f.sendIDs = f.sendIDs[1:]
	return id, nil
}

func (f *fakeTransport) EditMessage(chatID, messageID, text, format string) error {
	f.editCalls++
	f.editLog = append(f.editLog, fakeEditCall{chatID: chatID, message: messageID, text: text, format: format})
	f.lastEdit.chatID = chatID
	f.lastEdit.message = messageID
	f.lastEdit.text = text
	f.lastEdit.format = format
	if f.editErr != nil && format != "" {
		return f.editErr
	}
	return nil
}

func newTestDeliveryDaemon(tp transport.Transport) *daemon {
	return &daemon{
		transports: map[string]transport.Transport{"tg": tp},
		formats:    map[string]string{"tg": "html"},
		sendPause:  make(map[string]time.Time),
		sendFails:  make(map[string]int),
		chatEvents: make(map[string]uint64),
	}
}

func TestTryEditTreatsNotModifiedAsSuccess(t *testing.T) {
	tp := &fakeTransport{
		editErr: &transport.APIError{Platform: "tg", Code: 400, Description: "message is not modified"},
	}
	if err := tryEdit(context.Background(), tp, "chat", "msg", "same", "html"); err != nil {
		t.Fatalf("expected not modified to be ignored, got %v", err)
	}
	if tp.editCalls != 1 {
		t.Fatalf("expected one edit call, got %d", tp.editCalls)
	}
}

func TestTryEditFallsBackToPlainFormat(t *testing.T) {
	tp := &fakeTransport{
		editErr: &transport.APIError{Platform: "tg", Code: 400, Description: "bad format"},
	}
	err := tryEdit(context.Background(), tp, "chat", "msg", "<b>hello</b>", "html")
	if err != nil {
		t.Fatalf("expected plain fallback to succeed, got %v", err)
	}
	if tp.editCalls != 2 {
		t.Fatalf("expected formatted edit and plain fallback, got %d calls", tp.editCalls)
	}
	if tp.lastEdit.format != "" {
		t.Fatalf("expected last edit to use plain format, got %q", tp.lastEdit.format)
	}
}

func TestTrySendDropsReplyToWhenTargetGone(t *testing.T) {
	tp := &fakeTransport{
		sendErrFn: func(call fakeSendCall) error {
			if call.replyTo != "" {
				return &transport.APIError{Platform: "tg", Code: 400, Description: "Bad Request: message to be replied not found"}
			}
			return nil
		},
	}
	if err := trySend(context.Background(), tp, "chat", "missing-msg", "hello", "html"); err != nil {
		t.Fatalf("expected reply fallback to succeed, got %v", err)
	}
	if len(tp.sendLog) != 2 {
		t.Fatalf("expected initial send + reply-less retry, got %d calls: %+v", len(tp.sendLog), tp.sendLog)
	}
	if tp.sendLog[0].replyTo != "missing-msg" {
		t.Fatalf("expected first send to include reply, got %q", tp.sendLog[0].replyTo)
	}
	if tp.sendLog[1].replyTo != "" {
		t.Fatalf("expected fallback send without reply, got %q", tp.sendLog[1].replyTo)
	}
	if tp.sendLog[1].format != "html" {
		t.Fatalf("expected fallback to keep original format, got %q", tp.sendLog[1].format)
	}
}

func TestTrySendReturnIDDropsReplyToWhenTargetGone(t *testing.T) {
	tp := &fakeTransport{
		sendIDs: []string{"new-id"},
		sendErrFn: func(call fakeSendCall) error {
			if call.replyTo != "" {
				return &transport.APIError{Platform: "tg", Code: 400, Description: "Bad Request: message to be replied not found"}
			}
			return nil
		},
	}
	id, err := trySendReturnID(context.Background(), tp, "chat", "missing-msg", "hello", "html")
	if err != nil {
		t.Fatalf("expected reply fallback to succeed, got %v", err)
	}
	if id != "new-id" {
		t.Fatalf("expected new-id from successful fallback, got %q", id)
	}
	if len(tp.sendLog) != 2 {
		t.Fatalf("expected initial + fallback send, got %d", len(tp.sendLog))
	}
	if tp.sendLog[1].replyTo != "" {
		t.Fatalf("expected fallback without reply, got %q", tp.sendLog[1].replyTo)
	}
}

func TestTrySendKeepsReplyToOnUnrelatedError(t *testing.T) {
	tp := &fakeTransport{
		sendErrFn: func(call fakeSendCall) error {
			if call.format != "" {
				return &transport.APIError{Platform: "tg", Code: 400, Description: "Bad Request: can't parse entities"}
			}
			return nil
		},
	}
	if err := trySend(context.Background(), tp, "chat", "msg-1", "<b>hi</b>", "html"); err != nil {
		t.Fatalf("expected plain fallback to succeed, got %v", err)
	}
	if len(tp.sendLog) != 2 {
		t.Fatalf("expected formatted + plain retry, got %d", len(tp.sendLog))
	}
	for i, call := range tp.sendLog {
		if call.replyTo != "msg-1" {
			t.Fatalf("expected reply preserved on unrelated error, call %d got %q", i, call.replyTo)
		}
	}
}

func TestSyncMessageChainSkipsCachedEdits(t *testing.T) {
	tp := &fakeTransport{}
	d := newTestDeliveryDaemon(tp)
	ctx := context.Background()
	chain := newMessageChain()

	chain, err := d.syncMessageChain(ctx, "tg:1", "", chain, "hello", "html")
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if len(chain.ids) != 1 || chain.ids[0] == "" {
		t.Fatalf("expected one message id, got %v", chain.ids)
	}
	if tp.sendCalls != 1 {
		t.Fatalf("expected one send call, got %d", tp.sendCalls)
	}

	_, err = d.syncMessageChain(ctx, "tg:1", "", chain, "hello", "html")
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if tp.editCalls != 0 {
		t.Fatalf("expected cached sync to skip edits, got %d edit calls", tp.editCalls)
	}
}

func TestSyncMessageChainKeepsFollowupChunkUnrepliedWithoutGap(t *testing.T) {
	tp := &fakeTransport{sendIDs: []string{"first", "second"}}
	d := newTestDeliveryDaemon(tp)
	ctx := context.Background()

	chain, err := d.syncMessageChain(ctx, "tg:1", "user-msg", nil, "hello", "html")
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if len(tp.sendLog) != 1 || tp.sendLog[0].replyTo != "user-msg" {
		t.Fatalf("expected first chunk to reply to user, got %+v", tp.sendLog)
	}

	longText := strings.Repeat("chunk ", 900)
	_, err = d.syncMessageChain(ctx, "tg:1", "user-msg", chain, longText, "html")
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if len(tp.sendLog) < 2 {
		t.Fatalf("expected second send for appended chunk, got %d sends", len(tp.sendLog))
	}
	if got := tp.sendLog[1].replyTo; got != "" {
		t.Fatalf("expected appended chunk without reply after contiguous flow, got %q", got)
	}
}

func TestSyncMessageChainRepliesAfterInboundGap(t *testing.T) {
	tp := &fakeTransport{sendIDs: []string{"first", "second"}}
	d := newTestDeliveryDaemon(tp)
	ctx := context.Background()

	chain, err := d.syncMessageChain(ctx, "tg:1", "user-msg", nil, "hello", "html")
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	d.bumpChatActivity("tg:1")

	longText := strings.Repeat("chunk ", 900)
	_, err = d.syncMessageChain(ctx, "tg:1", "user-msg", chain, longText, "html")
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if len(tp.sendLog) < 2 {
		t.Fatalf("expected second send for appended chunk, got %d sends", len(tp.sendLog))
	}
	if got := tp.sendLog[1].replyTo; got != "user-msg" {
		t.Fatalf("expected appended chunk to reply after gap, got %q", got)
	}
}

func TestSyncMessageChainDoesNotReplyForAppendedChunkWhenReusingExistingMessageWithoutGap(t *testing.T) {
	tp := &fakeTransport{sendIDs: []string{"second"}}
	d := newTestDeliveryDaemon(tp)
	d.chatEvents["tg:1"] = 3
	ctx := context.Background()

	chain := newMessageChain("queued")
	chain.anchorReplyTo = "user-msg"
	chain.lastCreateActivity = 3

	longText := strings.Repeat("chunk ", 900)
	_, err := d.syncMessageChain(ctx, "tg:1", "user-msg", chain, longText, "html")
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if len(tp.sendLog) == 0 {
		t.Fatal("expected appended sends, got none")
	}
	for i, call := range tp.sendLog {
		if call.replyTo != "" {
			t.Fatalf("expected appended chunk %d without reply when no gap happened, got %q", i, call.replyTo)
		}
	}
}
