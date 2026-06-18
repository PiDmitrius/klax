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
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo hello\",\"yield_time_ms\":1000}"}}`,
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
	if items[1].Role != "assistant" || len(items[1].Tools) != 1 || items[1].Tools[0].Name != "Bash" {
		t.Fatalf("item1 = %+v", items[1])
	}
	if tc := items[1].Tools[0]; !strings.Contains(tc.Label, "Bash") || !strings.Contains(tc.Label, "echo hello") {
		t.Fatalf("codex tool label not enriched: %+v", tc)
	}
	if items[2].Role != "assistant" || items[2].Text != "doing X" {
		t.Fatalf("item2 = %+v", items[2])
	}
}

func TestReadCodexHistoryToolLabels(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Update File: /tmp/x.txt\n@@\n-old\n+new\n*** End Patch\n"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"update_plan","arguments":"{\"plan\":[{\"step\":\"one\",\"status\":\"completed\"},{\"step\":\"two\",\"status\":\"in_progress\"}]}"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"view_image","arguments":"{\"path\":\"/tmp/screen.png\",\"detail\":\"high\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"_create_issue","namespace":"mcp__codex_apps__github","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"tool_search_call","arguments":{"query":"GitHub create issue repository","limit":5}}}`,
		`{"type":"response_item","payload":{"type":"web_search_call","action":{"type":"search","query":"Codex history tool labels"}}}`,
		`{"type":"response_item","payload":{"type":"web_search_call","action":{"type":"find_in_page","url":"https://example.com/docs","pattern":"needle"}}}`,
		`{"type":"response_item","payload":{"type":"web_search_call","action":{"type":"search","queries":["fallback query"]}}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"pwd"}}`,
		`{"type":"item.started","item":{"type":"mcp_tool_call","server":"codex_apps","tool":"github_get_profile"}}`,
	})
	items, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 10 {
		t.Fatalf("want 10 items, got %d: %+v", len(items), items)
	}
	if tc := items[0].Tools[0]; tc.Name != "Edit" || !strings.Contains(tc.Label, "/tmp/x.txt") {
		t.Fatalf("patch tool label = %+v", tc)
	}
	if tc := items[1].Tools[0]; tc.Name != "Plan" || !strings.Contains(tc.Label, "two") || !strings.Contains(tc.Label, "1/2") {
		t.Fatalf("plan tool label = %+v", tc)
	}
	if tc := items[2].Tools[0]; tc.Name != "ViewImage" || !strings.Contains(tc.Label, "/tmp/screen.png") {
		t.Fatalf("image tool label = %+v", tc)
	}
	if tc := items[3].Tools[0]; tc.Name != "MCP" || !strings.Contains(tc.Label, "codex_apps.github._create_issue") {
		t.Fatalf("mcp tool label = %+v", tc)
	}
	if tc := items[4].Tools[0]; tc.Name != "ToolSearch" || !strings.Contains(tc.Label, "GitHub create issue") {
		t.Fatalf("tool search label = %+v", tc)
	}
	if tc := items[5].Tools[0]; tc.Name != "WebSearch" || !strings.Contains(tc.Label, "Codex history tool labels") {
		t.Fatalf("web search label = %+v", tc)
	}
	if tc := items[6].Tools[0]; tc.Name != "WebFind" || !strings.Contains(tc.Label, "needle") || !strings.Contains(tc.Label, "https://example.com/docs") {
		t.Fatalf("web find label = %+v", tc)
	}
	if tc := items[7].Tools[0]; tc.Name != "WebSearch" || !strings.Contains(tc.Label, "fallback query") {
		t.Fatalf("web search queries fallback label = %+v", tc)
	}
	if tc := items[8].Tools[0]; tc.Name != "Bash" || !strings.Contains(tc.Label, "pwd") {
		t.Fatalf("item.started command label = %+v", tc)
	}
	if tc := items[9].Tools[0]; tc.Name != "MCP" || !strings.Contains(tc.Label, "codex_apps.github_get_profile") {
		t.Fatalf("item.started mcp label = %+v", tc)
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
