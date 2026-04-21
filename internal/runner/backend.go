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

// Event is a unified event from any backend's JSON stream.
type Event struct {
	Type      string // "system", "tool", "text", "intermediate", "result", "unknown"
	SessionID string
	Model     string
	Tool      ToolUse
	Text      string
	Usage     ModelUsageInfo
	Error     string
	RateLimit *RateLimitInfo
}

// Backend abstracts the CLI tool that executes prompts (claude, codex, etc).
type Backend interface {
	// Name returns the backend identifier ("claude", "codex").
	Name() string

	// BuildCmd creates the exec.Cmd for a given request.
	BuildCmd(opts RunOptions) (*exec.Cmd, error)

	// ParseEvent parses a single line of JSON output into zero or more
	// unified events. A single backend line may carry multiple interleaved
	// content blocks (e.g. a Claude assistant message with text + tool_use +
	// text), and each becomes its own Event — preserving order within the
	// stream. Returns the events and true if the line was recognised, or
	// false/nil to skip.
	ParseEvent(line []byte) ([]Event, bool)
}
