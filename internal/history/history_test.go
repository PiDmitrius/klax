package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	var data string
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadClaude(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"mode","mode":"x"}`, // internal noise, ignored
		`{"type":"user","message":{"content":"hello"},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi there"},{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"out"}]}}`, // tool output fed back — no user text
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":100,"postTokens":10}}`,
		`{"type":"user","isSidechain":true,"message":{"content":"subagent"}}`, // sidechain — dropped by Parse
	})
	items, err := readClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %+v", len(items), items)
	}
	if items[0].Role != "user" || items[0].Text != "hello" {
		t.Fatalf("item0 = %+v", items[0])
	}
	if items[1].Role != "assistant" || items[1].Text != "hi there" || len(items[1].Tools) != 1 || items[1].Tools[0].Name != "Read" {
		t.Fatalf("item1 = %+v", items[1])
	}
	// Tool carries the rich label (matches live/Telegram), not just the bare name.
	if tc := items[1].Tools[0]; !strings.Contains(tc.Label, "Read") {
		t.Fatalf("tool label not enriched: %+v", tc)
	}
	if items[2].Role != "assistant" || items[2].Text != "done" {
		t.Fatalf("item2 = %+v", items[2])
	}
	if items[3].Role != "system" || items[3].Kind != "compact" {
		t.Fatalf("item3 = %+v", items[3])
	}
}

func TestReadCodex(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"session_meta","payload":{"id":"x"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"do X"}}`,
		`{"type":"response_item","payload":{"type":"reasoning","summary":[]}}`, // internal, ignored
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"doing X"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{}}}`, // meta, ignored
	})
	items, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(items), items)
	}
	if items[0].Role != "user" || items[0].Text != "do X" {
		t.Fatalf("item0 = %+v", items[0])
	}
	if items[1].Role != "assistant" || len(items[1].Tools) != 1 || items[1].Tools[0].Name != "exec_command" {
		t.Fatalf("item1 = %+v", items[1])
	}
	if items[2].Role != "assistant" || items[2].Text != "doing X" {
		t.Fatalf("item2 = %+v", items[2])
	}
}

func TestEncodeProjectDir(t *testing.T) {
	if got := encodeProjectDir("/home/claw"); got != "-home-claw" {
		t.Fatalf("encodeProjectDir = %q, want -home-claw", got)
	}
}

func TestReadCodexTimestamp(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"event_msg","timestamp":"2026-06-15T10:00:00.5Z","payload":{"type":"user_message","message":"hi"}}`,
	})
	items, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Time != "2026-06-15T10:00:00Z" {
		t.Fatalf("codex item time = %q (want 2026-06-15T10:00:00Z)", items[0].Time)
	}
}
