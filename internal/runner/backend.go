package runner

import (
	"os/exec"
)

// RateLimitInfo holds subscription rate limit status from a backend.
type RateLimitInfo struct {
	Status         string  // "allowed" | "allowed_warning" | "throttled" | "rejected"
	ResetsAt       int64   // unix timestamp
	RateLimitType  string  // "five_hour" | "seven_day"
	Utilization    float64 // 0.0–1.0
	IsUsingOverage bool
}

// EventType discriminates the unified Event stream the runner consumes from
// any backend. Constants are the only valid values — direct string literals
// in switch cases or emits are a typo waiting to happen.
type EventType string

const (
	// EventSystem carries session/model/rate-limit metadata. Emitted once at
	// startup and again whenever a rate-limit event arrives mid-stream.
	EventSystem EventType = "system"
	// EventTool is a tool invocation surfaced to the chat log.
	EventTool EventType = "tool"
	// EventText is a complete assistant text block; the runner paragraph-
	// joins consecutive EventText events with "\n\n".
	EventText EventType = "text"
	// EventTextDelta is a partial token-stream chunk; the runner raw-concats
	// these (no separator) — the model's own newlines provide structure.
	EventTextDelta EventType = "text_delta"
	// EventTextBoundary marks the end of a streamed text block. The runner
	// uses it to insert a paragraph break before the next text begins,
	// preserving what paragraph-join would have produced for EventText.
	// Runner-level concept, not tied to any single backend's wire format.
	EventTextBoundary EventType = "text_boundary"
	// EventIntermediate is codex-shaped completed text (one full agent_message
	// at a time); runner treats it the same as EventText.
	EventIntermediate EventType = "intermediate"
	// EventUnknown is a backend event we recognised but did not classify.
	// Surfaced as opaque progress for forward compatibility.
	EventUnknown EventType = "unknown"
	// EventError is a backend error item that should be visible in progress
	// without necessarily terminating the run; fatal turn errors use
	// EventResult.Error.
	EventError EventType = "error"
	// EventResult is the end-of-turn summary: final text (if no per-block
	// stream was emitted), usage totals, error info.
	EventResult EventType = "result"
)

// Event is a unified event from any backend's JSON stream.
type Event struct {
	Type      EventType
	SessionID string
	Model     string
	Tool      ToolUse
	Text      string
	Usage     ModelUsageInfo
	Error     string
	RateLimit *RateLimitInfo
}

// Backend abstracts the CLI tool that executes prompts (claude, codex, etc).
//
// Backend instances carry per-run state (e.g. ClaudeBackend tracks whether
// a turn has emitted partial deltas and whether a text block is currently
// streaming). Treat each instance as single-use per Run: do not share one
// instance across concurrent Run calls. The current daemon constructs a
// fresh backend per turn in cmd/klax/daemon.go:backendFor; new callers
// should do the same. BuildCmd is the natural place a backend resets its
// per-run state on entry.
type Backend interface {
	// Name returns the backend identifier ("claude", "codex").
	Name() string

	// BuildCmd creates the exec.Cmd for a given request and resets any
	// per-run parser state on the backend instance.
	BuildCmd(opts RunOptions) (*exec.Cmd, error)

	// ParseEvent parses a single line of JSON output into zero or more
	// unified events. A single backend line may carry multiple interleaved
	// content blocks (e.g. a Claude assistant message with text + tool_use +
	// text), and each becomes its own Event — preserving order within the
	// stream. Returns the events and true if the line was recognised, or
	// false/nil to skip. May mutate per-run state on the backend.
	ParseEvent(line []byte) ([]Event, bool)
}
