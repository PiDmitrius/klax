package main

import (
	"strings"
	"testing"
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
