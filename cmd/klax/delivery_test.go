package main

import (
	"context"
	"strings"
	"testing"
	"time"

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

type fakeTransport struct {
	sendIDs   []string
	sendCalls int
	editCalls int
	editErr   error
	lastEdit  struct {
		chatID   string
		message  string
		text     string
		format   string
	}
}

func (f *fakeTransport) SendMessage(chatID, text, replyTo, format string) error {
	f.sendCalls++
	return nil
}

func (f *fakeTransport) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	f.sendCalls++
	if len(f.sendIDs) == 0 {
		return "generated-id", nil
	}
	id := f.sendIDs[0]
	f.sendIDs = f.sendIDs[1:]
	return id, nil
}

func (f *fakeTransport) EditMessage(chatID, messageID, text, format string) error {
	f.editCalls++
	f.lastEdit.chatID = chatID
	f.lastEdit.message = messageID
	f.lastEdit.text = text
	f.lastEdit.format = format
	if f.editErr != nil && format != "" {
		return f.editErr
	}
	return nil
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

func TestSyncMessageChainSkipsCachedEdits(t *testing.T) {
	d := &daemon{
		editCache: make(map[string]string),
		sendPause: make(map[string]time.Time),
		sendFails: make(map[string]int),
	}
	tp := &fakeTransport{}
	ctx := context.Background()

	ids, err := d.syncMessageChain(ctx, "tg:1", tp, "1", "", nil, "hello", "html")
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("expected one message id, got %v", ids)
	}
	if tp.sendCalls != 1 {
		t.Fatalf("expected one send call, got %d", tp.sendCalls)
	}

	_, err = d.syncMessageChain(ctx, "tg:1", tp, "1", "", ids, "hello", "html")
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if tp.editCalls != 0 {
		t.Fatalf("expected cached sync to skip edits, got %d edit calls", tp.editCalls)
	}
}
