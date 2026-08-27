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

func TestCodexTerminalErrorComesFromRollout(t *testing.T) {
	line := []byte(`{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"Selected model is at capacity. Please try a different model.","codex_error_info":"server_overloaded"}}}`)
	want := "Selected model is at capacity. Please try a different model. (server_overloaded)"
	if got := ParseCodexTerminalError(line); got != want {
		t.Fatalf("terminal error = %q, want %q", got, want)
	}
}

func TestLatestSuccessfulCodexTurnClearsEarlierError(t *testing.T) {
	failure := []byte(`{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"overloaded","codex_error_info":"server_overloaded"}}}`)
	success := []byte(`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done","error":null}}`)
	if got, complete := parseCodexTaskComplete(failure); !complete || got == "" {
		t.Fatalf("failure not recognized: complete=%v error=%q", complete, got)
	}
	if got, complete := parseCodexTaskComplete(success); !complete || got != "" {
		t.Fatalf("success must clear an earlier error: complete=%v error=%q", complete, got)
	}
	data := append(append(append([]byte{}, failure...), '\n'), success...)
	data = append(data, '\n')
	if got := latestCodexTaskComplete(data); got != "" {
		t.Fatalf("latest successful turn retained stale error: %q", got)
	}
}

func TestCurrentCodexTurnDoesNotInheritPriorError(t *testing.T) {
	prior := []byte(`{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"old failure"}}}` + "\n")
	current := []byte(`{"type":"event_msg","payload":{"type":"token_count"}}` + "\n")
	rollout := append(append([]byte{}, prior...), current...)
	if _, complete := parseCodexTaskComplete(current); complete {
		t.Fatal("a non task_complete record was treated as a complete turn")
	}
	if got := latestCodexTaskComplete(rollout[len(prior):]); got != "" {
		t.Fatalf("current turn inherited prior terminal error: %q", got)
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
