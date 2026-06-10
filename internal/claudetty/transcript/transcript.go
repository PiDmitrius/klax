// Package transcript reads a Claude Code session transcript (JSONL) while
// the child `claude` is still writing it, and converts transcript lines to
// `claude -p --output-format stream-json` wire format.
//
// The two formats are siblings, not twins:
//   - transcript assistant lines carry the same `message` object stream-json
//     does, but the session id field is `sessionId` (camelCase) and there is
//     no `system` init line or trailing `result` envelope;
//   - transcripts also contain line types stream-json never emits
//     (`summary`, `file-history-snapshot`, `progress`, queued-command echoes)
//     which must not reach a stream-json consumer.
//
// Convert filters and rewrites; Tailer feeds it incrementally.
package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// Line is one parsed transcript line, holding only the fields claudetty
// routes on. Raw retains the original bytes for pass-through.
type Line struct {
	Type       string
	SessionID  string
	IsAPIError bool
	Error      string
	Raw        json.RawMessage
}

type rawLine struct {
	Type              string `json:"type"`
	SessionID         string `json:"sessionId"`
	SessionIDp        string `json:"session_id"`
	IsSidechain       bool   `json:"isSidechain"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
}

// Parse parses a single transcript line. ok=false for malformed/empty lines.
func Parse(line []byte) (Line, bool) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return Line{}, false
	}
	var r rawLine
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return Line{}, false
	}
	sid := r.SessionID
	if sid == "" {
		sid = r.SessionIDp
	}
	if r.IsSidechain {
		// Subagent traffic — never part of the top-level stream-json.
		return Line{}, false
	}
	return Line{
		Type:       r.Type,
		SessionID:  sid,
		IsAPIError: r.IsAPIErrorMessage,
		Error:      r.Error,
		Raw:        json.RawMessage(trimmed),
	}, true
}

// Usage aggregates token counts across assistant messages.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// Summary accumulates what the final `result` envelope needs. Feed it every
// parsed line; read the fields after the Stop hook.
type Summary struct {
	SessionID string
	Model     string
	FinalText string
	IsError   bool
	NumTurns  int
	Usage     Usage
}

type assistantMsg struct {
	Message struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Add folds one parsed line into the summary.
func (s *Summary) Add(l Line) {
	if s.SessionID == "" && l.SessionID != "" {
		s.SessionID = l.SessionID
	}
	if l.IsAPIError {
		s.IsError = true
		// Surface the API error as the turn's final text. Without this the
		// driver keeps waiting for a Stop hook that an errored turn may never
		// deliver, then on process exit masks the real cause with a generic
		// "exited unexpectedly" message.
		if l.Error != "" {
			s.FinalText = l.Error
		}
	}
	if l.Type != "assistant" {
		return
	}
	var m assistantMsg
	if err := json.Unmarshal(l.Raw, &m); err != nil {
		return
	}
	s.NumTurns++
	if m.Message.Model != "" {
		s.Model = m.Message.Model
	}
	// Last assistant message wins; concatenate its text blocks.
	var text strings.Builder
	for _, b := range m.Message.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if text.Len() > 0 {
		s.FinalText = text.String()
	}
	if u := m.Message.Usage; u != nil {
		s.Usage.InputTokens += u.InputTokens
		s.Usage.OutputTokens += u.OutputTokens
		s.Usage.CacheReadTokens += u.CacheReadTokens
		s.Usage.CacheCreationTokens += u.CacheCreationTokens
	}
}

// Tailer incrementally reads a growing JSONL file. It keeps its own offset
// (the writer's offset is never disturbed — reads use ReadAt) and holds back
// incomplete trailing fragments until the newline arrives, so callers never
// see torn JSON.
type Tailer struct {
	file    *os.File
	pos     int64
	partial []byte
	buf     []byte // reused read buffer; allocated lazily on first Pump
	frozen  bool   // once set, Pump stops resetting on shrink (see Freeze)
}

// OpenTailer opens path for tailing. Fails with os.ErrNotExist until the
// child actually creates the file — callers retry.
func OpenTailer(path string) (*Tailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Tailer{file: f}, nil
}

// Close releases the file handle.
func (t *Tailer) Close() {
	t.file.Close()
}

// Freeze disables Pump's shrink-recovery. Call it once the consumer has begun
// emitting this turn's lines: after that point a file that auto-compact
// rewrites shorter must NOT trigger a re-read from the top, or every already-
// emitted line would be replayed to the consumer. Appends past the current
// offset are still read — only the reset-on-shrink is turned off.
func (t *Tailer) Freeze() {
	t.frozen = true
}

// Pump reads newly-appended bytes and returns each complete line (without
// the trailing newline). Returns nil when nothing new is available.
func (t *Tailer) Pump() [][]byte {
	// Auto-compact can rewrite the transcript shorter. If the file is now
	// smaller than our read offset it was truncated/replaced under us;
	// ReadAt would otherwise sit past EOF forever and never see the new
	// content. Restart from the top so we can re-find this turn's prompt
	// echo. Suppressed once Frozen: post-echo a reset would replay every
	// already-emitted line (see Freeze).
	if !t.frozen {
		if fi, err := t.file.Stat(); err == nil && fi.Size() < t.pos {
			t.pos = 0
			t.partial = t.partial[:0]
		}
	}
	if t.buf == nil {
		t.buf = make([]byte, 65536)
	}
	var lines [][]byte
	for {
		n, err := t.file.ReadAt(t.buf, t.pos)
		if n > 0 {
			t.pos += int64(n)
			t.partial = append(t.partial, t.buf[:n]...)
		}
		for {
			nl := bytes.IndexByte(t.partial, '\n')
			if nl < 0 {
				break
			}
			line := make([]byte, nl)
			copy(line, t.partial[:nl])
			lines = append(lines, line)
			t.partial = t.partial[nl+1:]
		}
		if err != nil || n < len(t.buf) {
			break
		}
	}
	return lines
}
