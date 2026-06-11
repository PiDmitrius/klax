package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAssistantLine(t *testing.T) {
	raw := []byte(`{"type":"assistant","sessionId":"s1","message":{"model":"m","content":[{"type":"text","text":"hello"}]}}`)
	line, ok := Parse(raw)
	if !ok {
		t.Fatal("line rejected")
	}
	if line.Type != "assistant" || line.SessionID != "s1" || string(line.Raw) != string(raw) {
		t.Fatalf("line = %+v raw=%s", line, line.Raw)
	}
}

func TestParseFiltersSidechain(t *testing.T) {
	if _, ok := Parse([]byte(`{"type":"assistant","sessionId":"s1","isSidechain":true}`)); ok {
		t.Fatal("sidechain line accepted")
	}
}

func TestParseCompactBoundary(t *testing.T) {
	line, ok := Parse([]byte(`{"type":"system","subtype":"compact_boundary","sessionId":"s1","compactMetadata":{"trigger":"manual","preTokens":144630,"postTokens":7315}}`))
	if !ok {
		t.Fatal("compact_boundary rejected")
	}
	if line.Type != "system" || line.Subtype != "compact_boundary" {
		t.Fatalf("line = %+v", line)
	}
	if line.Compact == nil || line.Compact.Trigger != "manual" ||
		line.Compact.PreTokens != 144630 || line.Compact.PostTokens != 7315 {
		t.Fatalf("compact = %+v", line.Compact)
	}
}

func TestSummaryAddAssistant(t *testing.T) {
	var s Summary
	line, ok := Parse([]byte(`{"type":"assistant","sessionId":"s1","message":{"model":"model-a","content":[{"type":"text","text":"hi"},{"type":"text","text":" there"}],"usage":{"input_tokens":2,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}}`))
	if !ok {
		t.Fatal("line rejected")
	}
	s.Add(line)
	if s.SessionID != "s1" || s.Model != "model-a" || s.FinalText != "hi there" || s.NumTurns != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.Usage.InputTokens != 2 || s.Usage.OutputTokens != 3 || s.Usage.CacheReadTokens != 5 || s.Usage.CacheCreationTokens != 7 {
		t.Fatalf("usage = %+v", s.Usage)
	}
}

func TestSummaryMarksAPIError(t *testing.T) {
	var s Summary
	line, ok := Parse([]byte(`{"type":"assistant","sessionId":"s1","isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"limited"}]}}`))
	if !ok {
		t.Fatal("line rejected")
	}
	s.Add(line)
	if !s.IsError || s.FinalText != "limited" {
		t.Fatalf("summary = %+v", s)
	}
}

func TestTailerHoldsPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"summary"}`+"\n"+`{"type":"assistant"`), 0o644); err != nil {
		t.Fatal(err)
	}
	tailer, err := OpenTailer(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tailer.Close()

	lines := tailer.Pump()
	if len(lines) != 1 || string(lines[0]) != `{"type":"summary"}` {
		t.Fatalf("lines = %q", lines)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`,"sessionId":"s1"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines = tailer.Pump()
	if len(lines) != 1 || string(lines[0]) != `{"type":"assistant","sessionId":"s1"}` {
		t.Fatalf("lines = %q", lines)
	}
}

func TestParseLineTimestamp(t *testing.T) {
	l, ok := Parse([]byte(`{"type":"system","subtype":"compact_boundary","timestamp":"2026-06-11T06:32:00.631Z","compactMetadata":{"trigger":"manual","preTokens":212091,"postTokens":7841}}`))
	if !ok {
		t.Fatal("line rejected")
	}
	want := time.Date(2026, 6, 11, 6, 32, 0, 631_000_000, time.UTC)
	if !l.Time.Equal(want) {
		t.Fatalf("Time = %v, want %v", l.Time, want)
	}

	l, ok = Parse([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`))
	if !ok {
		t.Fatal("line rejected")
	}
	if !l.Time.IsZero() {
		t.Fatalf("Time = %v, want zero when the line has no timestamp", l.Time)
	}

	l, ok = Parse([]byte(`{"type":"user","timestamp":"not-a-time"}`))
	if !ok {
		t.Fatal("line rejected")
	}
	if !l.Time.IsZero() {
		t.Fatalf("Time = %v, want zero for an unparseable timestamp", l.Time)
	}
}
