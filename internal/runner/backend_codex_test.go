package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTokenCountUsesLastUsageForContext(t *testing.T) {
	line := []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":26126634},"last_token_usage":{"input_tokens":142257},"model_context_window":258400}}}`)
	var meta codexSessionMeta
	if !parseCodexSessionMetaLine(line, &meta) {
		t.Fatal("token_count line did not update meta")
	}
	if meta.ContextUsed != 142257 {
		t.Fatalf("ContextUsed = %d, want last_token_usage.input_tokens 142257", meta.ContextUsed)
	}
	if meta.ContextWindow != 258400 {
		t.Fatalf("ContextWindow = %d, want 258400", meta.ContextWindow)
	}
}

// Poll must not swallow an unterminated final line: when codex is mid-write, the trailing
// partial token_count is reparsed from its start on the next poll instead of being skipped.
func TestCodexMetaTailReparsesUnterminatedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	full := `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100},"model_context_window":200000}}}` + "\n"
	partial := `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":` // mid-write, no newline
	if err := os.WriteFile(path, []byte(full+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := &codexSessionMetaTail{path: path}
	if meta, changed := tail.Poll(); !changed || meta.ContextUsed != 100 {
		t.Fatalf("poll 1: changed=%v used=%d, want changed with used 100", changed, meta.ContextUsed)
	}
	// codex finishes writing the previously-partial line
	if err := os.WriteFile(path, []byte(full+partial+`142000},"model_context_window":258400}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, changed := tail.Poll()
	if !changed || meta.ContextUsed != 142000 || meta.ContextWindow != 258400 {
		t.Fatalf("poll 2: changed=%v used=%d win=%d, want 142000/258400 (line reparsed, not skipped)", changed, meta.ContextUsed, meta.ContextWindow)
	}
}
