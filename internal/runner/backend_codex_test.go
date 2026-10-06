package runner

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAdditionalInstructionsOnNewAndResumedRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, id := range []string{"", "thread-1"} {
		instructions := "Use \"quotes\",\nUnicode 😀 and control \x01."
		cmd, err := (&CodexBackend{}).BuildCmd(RunOptions{SessionID: id, AppendSystemPrompt: instructions, Prompt: "request"})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i, arg := range cmd.Args {
			if !strings.HasPrefix(arg, "developer_instructions=") {
				continue
			}
			var decoded string
			if i == 0 || cmd.Args[i-1] != "-c" || json.Unmarshal([]byte(strings.TrimPrefix(arg, "developer_instructions=")), &decoded) != nil || decoded != instructions {
				t.Fatalf("invalid instructions argument: %q", cmd.Args)
			}
			found = true
		}
		prompt, err := io.ReadAll(cmd.Stdin)
		if !found || err != nil || string(prompt) != "request" {
			t.Fatalf("instructions missing or user prompt changed: %q, %v", prompt, err)
		}
	}
}

func TestCodexTokenCountCarriesContextWindow(t *testing.T) {
	line := []byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":26126634},"last_token_usage":{"input_tokens":142257},"model_context_window":258400}}}`)
	var meta codexSessionMeta
	parseCodexSessionMetaLine(line, &meta)
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

// writeRollout puts a rollout for thread under a fresh HOME and returns its path.
func writeRollout(t *testing.T, thread, data string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "30")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLatestSuccessfulCodexTurnClearsEarlierError(t *testing.T) {
	failure := `{"type":"event_msg","payload":{"type":"task_complete","error":{"message":"overloaded","codex_error_info":"server_overloaded"}}}`
	success := `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done","error":null}}`
	if got, complete := parseCodexTaskComplete([]byte(success)); !complete || got != "" {
		t.Fatalf("success must clear an earlier error: complete=%v error=%q", complete, got)
	}
	writeRollout(t, "t1", failure+"\n"+success+"\n")
	if got, _ := readCodexRun("t1", 0); got != "" {
		t.Fatalf("latest successful turn retained stale error: %q", got)
	}
}

// A run reads only the rollout bytes it appended: an older turn's error, model and window are
// neither inherited nor reported, and an unfinished trailing record is ignored.
func TestCodexRunReadsOnlyItsOwnBytes(t *testing.T) {
	prior := `{"timestamp":"2026-09-30T10:00:00Z","type":"turn_context","payload":{"turn_id":"a","model":"old"}}` + "\n" +
		`{"timestamp":"2026-09-30T10:00:01Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"old failure"}}}` + "\n"
	current := `{"timestamp":"2026-09-30T10:01:00Z","type":"event_msg","payload":{"type":"task_started","model_context_window":258400}}` + "\n" +
		`{"timestamp":"2026-09-30T10:01:01Z","type":"response_item","payload":{"type":"function_call_output","output":"{\\"type\\":\\"turn_context\\"}"}}` + "\n" +
		`{"payload":{"model":"new"},"type":"turn_context"}` + "\n" +
		`{"timestamp":"2026-09-30T10:01:02Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"half`
	writeRollout(t, "t2", prior+current)
	if terminal, _ := readCodexRun("t2", 0); terminal != "old failure" {
		t.Fatalf("whole rollout: terminal %q, want the prior turn's", terminal)
	}
	terminal, meta := readCodexRun("t2", int64(len(prior)))
	if terminal != "" || meta.Model != "new" || meta.ContextWindow != 258400 {
		t.Fatalf("run = %q %+v, want no error, model new, window 258400", terminal, meta)
	}
}

func TestCodexLeadReadsFixedKeyOrderOnly(t *testing.T) {
	for line, want := range map[string][3]string{
		`{"timestamp":"t","ordinal":3,"type":"event_msg","payload":{"type":"item_completed","thread_id":"a","turn_id":"b","item":{"type":"AgentMessage"}}}`: {"event_msg", "item_completed", "AgentMessage"},
		`{"timestamp":"t","type":"turn_context","payload":{"turn_id":"b","model":"m"}}`:                                                                     {"turn_context", "", ""},
	} {
		r, p, it, ok := CodexLead([]byte(line))
		if !ok || [3]string{r, p, it} != want {
			t.Errorf("CodexLead(%s) = %q %q %q %v, want %q", line, r, p, it, ok, want)
		}
	}
	if _, _, _, ok := CodexLead([]byte(`{"type":"event_msg","timestamp":"t"}`)); ok {
		t.Error("a record in another key order must be decoded, not classified by its lead")
	}
}

// An event whose payload does not start with its type is decoded, not skipped by its lead.
func TestCodexRunDecodesUnorderedPayload(t *testing.T) {
	writeRollout(t, "t3", `{"timestamp":"t","type":"event_msg","payload":{"turn_id":"r","type":"task_complete","error":{"message":"failed"}}}`+"\n")
	if terminal, _ := readCodexRun("t3", 0); terminal != "failed" {
		t.Fatalf("terminal = %q, want failed", terminal)
	}
}
