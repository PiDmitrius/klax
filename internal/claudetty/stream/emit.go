// Package stream converts parsed transcript lines into the
// `claude -p --output-format stream-json` wire format klax consumes:
//
//   - one synthesized `system`/`init` line carrying the session id,
//   - `assistant` lines passed through (their `message` object is already
//     wire-compatible; transcript-only line types are dropped),
//   - one trailing `result` envelope with aggregate usage keyed by model.
package stream

import (
	"encoding/json"
	"io"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
)

type Emitter struct {
	w        io.Writer
	initSent bool
}

func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w}
}

func (e *Emitter) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.w.Write(append(b, '\n'))
}

// Init emits the system/init line once. Call as soon as the session id is
// known — klax binds new sessions to the id from this line.
func (e *Emitter) Init(s *transcript.Summary) {
	if e.initSent {
		return
	}
	e.initSent = true
	e.emit(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": s.SessionID,
		"model":      s.Model,
	})
}

// Line forwards one transcript line if it belongs on the wire.
func (e *Emitter) Line(l transcript.Line, s *transcript.Summary) {
	switch l.Type {
	case "assistant":
		e.Init(s) // safety net if no hook carried the session id
		e.w.Write(append([]byte(l.Raw), '\n'))
	case "system":
		// Forward a context-compaction boundary as a slim stream-json line.
		// The transcript's own line also carries the large preserved segment,
		// which the wire consumer does not need; other system transcript lines
		// are not part of the stream-json contract klax parses.
		if l.Subtype == "compact_boundary" && l.Compact != nil {
			e.Init(s)
			e.emit(map[string]any{
				"type":    "system",
				"subtype": "compact_boundary",
				"compactMetadata": map[string]any{
					"trigger":    l.Compact.Trigger,
					"preTokens":  l.Compact.PreTokens,
					"postTokens": l.Compact.PostTokens,
				},
			})
		}
	default:
		// user, summary, file-history-snapshot, progress…
		// — not part of the stream-json contract klax parses.
	}
}

// Result emits the trailing result envelope.
func (e *Emitter) Result(s *transcript.Summary, duration time.Duration) {
	subtype := "success"
	if s.IsError {
		subtype = "error"
	}
	model := s.Model
	if model == "" {
		model = "unknown"
	}
	e.emit(map[string]any{
		"type":        "result",
		"subtype":     subtype,
		"is_error":    s.IsError,
		"result":      s.FinalText,
		"session_id":  s.SessionID,
		"num_turns":   s.NumTurns,
		"duration_ms": duration.Milliseconds(),
		"usage": map[string]any{
			"input_tokens":                s.Usage.InputTokens,
			"output_tokens":               s.Usage.OutputTokens,
			"cache_read_input_tokens":     s.Usage.CacheReadTokens,
			"cache_creation_input_tokens": s.Usage.CacheCreationTokens,
		},
		"modelUsage": map[string]any{
			model: map[string]any{
				"inputTokens":              s.Usage.InputTokens,
				"outputTokens":             s.Usage.OutputTokens,
				"cacheReadInputTokens":     s.Usage.CacheReadTokens,
				"cacheCreationInputTokens": s.Usage.CacheCreationTokens,
				"contextWindow":            0,
			},
		},
	})
}
