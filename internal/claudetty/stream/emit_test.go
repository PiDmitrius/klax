package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
)

func TestEmitterWireContract(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	summary := transcript.Summary{SessionID: "s1", Model: "m"}

	em.Init(&summary)
	line, ok := transcript.Parse([]byte(`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"ok"}]}}`))
	if !ok {
		t.Fatal("assistant rejected")
	}
	summary.Add(line)
	em.Line(line, &summary)
	em.Result(&summary, 1500*time.Millisecond)

	sc := bufio.NewScanner(&buf)
	var got []map[string]any
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		got = append(got, obj)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("emitted %d lines: %#v", len(got), got)
	}
	if got[0]["type"] != "system" || got[0]["subtype"] != "init" || got[0]["session_id"] != "s1" {
		t.Fatalf("init = %#v", got[0])
	}
	if got[1]["type"] != "assistant" {
		t.Fatalf("assistant = %#v", got[1])
	}
	if got[2]["type"] != "result" || got[2]["subtype"] != "success" || got[2]["result"] != "ok" {
		t.Fatalf("result = %#v", got[2])
	}
}

func TestEmitterDropsTranscriptOnlyLines(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	summary := transcript.Summary{SessionID: "s1"}
	line, ok := transcript.Parse([]byte(`{"type":"summary","sessionId":"s1","summary":"skip"}`))
	if !ok {
		t.Fatal("summary rejected")
	}
	em.Line(line, &summary)
	if buf.Len() != 0 {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestEmitterForwardsCompactBoundary(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	summary := transcript.Summary{SessionID: "s1"}
	line, ok := transcript.Parse([]byte(`{"type":"system","subtype":"compact_boundary","sessionId":"s1","compactMetadata":{"trigger":"manual","preTokens":144630,"postTokens":7315},"preservedSegment":{"fat":"payload"}}`))
	if !ok {
		t.Fatal("compact_boundary rejected")
	}
	em.Line(line, &summary)

	sc := bufio.NewScanner(&buf)
	var got []map[string]any
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		got = append(got, obj)
	}
	// init line (session id) followed by the slim compact line.
	if len(got) != 2 {
		t.Fatalf("emitted %d lines: %#v", len(got), got)
	}
	cb := got[1]
	if cb["type"] != "system" || cb["subtype"] != "compact_boundary" {
		t.Fatalf("compact line = %#v", cb)
	}
	if _, leaked := cb["preservedSegment"]; leaked {
		t.Fatalf("preservedSegment leaked onto the wire: %#v", cb)
	}
	cm, _ := cb["compactMetadata"].(map[string]any)
	if cm == nil || cm["trigger"] != "manual" ||
		cm["preTokens"].(float64) != 144630 || cm["postTokens"].(float64) != 7315 {
		t.Fatalf("compactMetadata = %#v", cb["compactMetadata"])
	}
}
