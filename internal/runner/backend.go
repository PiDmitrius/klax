package runner

import (
	"os/exec"
)

// RateLimitInfo holds subscription rate limit status from a backend.
type RateLimitInfo struct {
	Status         string // "allowed" | "throttled"
	ResetsAt       int64  // unix timestamp
	RateLimitType  string // "five_hour"
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

	// ParseEvent parses a single line of JSON output into a unified Event.
	// Returns the event and true if the line was parsed, or false to skip.
	ParseEvent(line []byte) (Event, bool)
}
