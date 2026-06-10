package runner

import "testing"

// TestParseEventToleratesTranscriptAssistantEnvelope locks the central
// compatibility bet of klax tty mode: the driver forwards assistant lines with
// the *transcript* envelope (top-level `sessionId` camelCase plus `uuid`,
// `parentUuid`, `timestamp`, `cwd`) rather than the `claude -p` stream-json
// envelope (`session_id` snake_case, `parent_tool_use_id`). The runner must
// read only `.message` off assistant lines; if it ever validated the envelope,
// every tty turn would break.
func TestParseEventToleratesTranscriptAssistantEnvelope(t *testing.T) {
	line := []byte(`{"type":"assistant","sessionId":"s-1","uuid":"u-1","parentUuid":"p-0",` +
		`"timestamp":"2026-06-10T00:00:00Z","cwd":"/home/x",` +
		`"message":{"model":"claude-fable-5","content":[{"type":"text","text":"PONG"}],` +
		`"usage":{"input_tokens":3,"cache_read_input_tokens":7,"output_tokens":1}}}`)

	evs, ok := (&ClaudeBackend{}).ParseEvent(line)
	if !ok {
		t.Fatal("transcript-enveloped assistant line not recognised")
	}
	var text string
	var ctxUsed int
	for _, ev := range evs {
		if ev.Type == EventText {
			text = ev.Text
			ctxUsed = ev.Usage.ContextUsed
		}
	}
	if text != "PONG" {
		t.Fatalf("text = %q, want PONG", text)
	}
	// Usage still reads off `.message.usage` regardless of the outer envelope.
	if ctxUsed != 3+7 {
		t.Fatalf("ContextUsed = %d, want 10 (input+cache_read)", ctxUsed)
	}
}
