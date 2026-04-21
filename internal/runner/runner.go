// Package runner executes AI CLI tools and streams results.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/fmtutil"
	"github.com/PiDmitrius/klax/internal/pathutil"
)

// killGracePeriod is how long we wait for a cancelled process group to exit
// on its own after SIGTERM before following up with SIGKILL. Tune if backends
// start doing meaningful cleanup on shutdown.
const killGracePeriod = 3 * time.Second

// ToolUse describes what the AI is doing right now.
type ToolUse struct {
	Name  string
	Input string
}

const toolPreviewLimit = 72

func (t ToolUse) String() string {
	switch t.Name {
	case "Bash":
		var inp struct{ Command string }
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("⚙️ Bash: `%s`", truncate(inp.Command, toolPreviewLimit))
	case "Read":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📖 Read: %s", inp.FilePath)
	case "Edit", "MultiEdit":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("✏️ Edit: %s", inp.FilePath)
	case "Write":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📝 Write: %s", inp.FilePath)
	case "WebFetch":
		var inp struct {
			URL string `json:"url"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🌐 Fetch: %s", truncate(inp.URL, toolPreviewLimit))
	case "WebSearch":
		var inp struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Query != "" {
			return fmt.Sprintf("🔎 Search: %s", truncate(inp.Query, toolPreviewLimit))
		}
		return "🔎 Search..."
	case "Glob", "GlobTool":
		var inp struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🔍 Glob: %s", inp.Pattern)
	case "Grep", "GrepTool":
		var inp struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🔍 Grep: %s", inp.Pattern)
	case "LS":
		var inp struct {
			Path string `json:"path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📂 LS: %s", inp.Path)
	case "Task":
		return "🤖 Task"
	case "TodoWrite":
		return "📋 TodoWrite"
	case "MCP":
		var inp struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		label := inp.Tool
		if inp.Server != "" && inp.Tool != "" {
			label = inp.Server + "." + inp.Tool
		} else if inp.Server != "" {
			label = inp.Server
		}
		if label == "" {
			return "🔌 MCP"
		}
		return fmt.Sprintf("🔌 MCP: %s", label)
	default:
		return fmt.Sprintf("🔧 %s", t.Name)
	}
}

func formatRateLimit(rl *RateLimitInfo) string {
	typeLabel := ""
	switch rl.RateLimitType {
	case "five_hour":
		typeLabel = "5ч"
	case "weekly", "seven_day":
		typeLabel = "нед"
	default:
		typeLabel = rl.RateLimitType
	}
	remaining := ""
	if rl.ResetsAt > 0 {
		d := time.Until(time.Unix(rl.ResetsAt, 0))
		if d > 0 {
			remaining = " " + fmtutil.Duration(d)
		}
	}
	switch rl.Status {
	case "throttled", "rejected":
		s := fmt.Sprintf("🚫 Лимит (%s)%s", typeLabel, remaining)
		if rl.IsUsingOverage {
			s += " (overage)"
		}
		return s
	case "allowed_warning":
		pct := int(rl.Utilization * 100)
		return fmt.Sprintf("⚠️ Лимит (%s) %d%%%s", typeLabel, pct, remaining)
	default:
		return fmt.Sprintf("⏱ Лимит (%s) %s%s", typeLabel, rl.Status, remaining)
	}
}


func truncate(s string, n int) string {
	s = pathutil.TildePathsInText(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RunOptions configures a CLI invocation.
type RunOptions struct {
	Prompt             string
	SessionID          string // empty = new session
	CWD                string // working directory
	Sandbox            string // "on" = CLI defaults, "off" = unrestricted
	Model              string // model override
	Effort             string // reasoning effort: low | medium | high (claude also: max; codex also: xhigh)
	AppendSystemPrompt string // appended to default system prompt
}

// ModelUsageInfo captures context window usage from a run.
type ModelUsageInfo struct {
	Model         string
	ContextWindow int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreation int
	ContextUsed   int
}

// RunResult is the final result of a CLI invocation.
type RunResult struct {
	SessionID string
	Text      string
	Usage     ModelUsageInfo
	RateLimit *RateLimitInfo
	Error     error
}

// ProgressKind discriminates progress events surfaced during a run.
type ProgressKind string

const (
	// ProgressKindTool is a tool invocation label ("⚙️ Bash: ls ~").
	ProgressKindTool ProgressKind = "tool"
	// ProgressKindNarration is an assistant text block that turned out not
	// to be the final answer (another text block came after it). Frontends
	// should render it distinctly from the final answer body.
	ProgressKindNarration ProgressKind = "narration"
)

// ProgressEvent is a single streamed progress update.
type ProgressEvent struct {
	Kind ProgressKind
	Text string
}

// ProgressFunc is called with human-readable progress updates.
type ProgressFunc func(ev ProgressEvent)

// Runner executes an AI backend and tracks state.
type Runner struct {
	mu      sync.Mutex
	busy    bool
	current ToolUse
	startAt time.Time
	toolAt  time.Time
}

func New() *Runner {
	return &Runner{}
}

func (r *Runner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

// Status returns current tool, time since tool started, and total run time.
func (r *Runner) Status() (tool ToolUse, toolElapsed, totalElapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := time.Since(r.startAt)
	var te time.Duration
	if r.current.Name != "" {
		te = time.Since(r.toolAt)
	}
	return r.current, te, total
}

// Run executes the backend with streaming output. Cancelling ctx sends SIGTERM
// to the backend's process group, then SIGKILL after killGracePeriod, and
// closes the stdout pipe so the scanner loop unblocks even if children still
// hold write-ends (e.g. rust grandchild of the codex npm shim).
func (r *Runner) Run(ctx context.Context, backend Backend, opts RunOptions, onProgress ProgressFunc) RunResult {
	r.mu.Lock()
	r.busy = true
	r.startAt = time.Now()
	r.current = ToolUse{}
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.busy = false
		r.current = ToolUse{}
		r.mu.Unlock()
	}()

	cmd, err := backend.BuildCmd(opts)
	if err != nil {
		return RunResult{Error: err}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{Error: fmt.Errorf("pipe: %w", err)}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return RunResult{Error: fmt.Errorf("start: %w", err)}
	}

	stopWatcher := watchCancel(ctx, cmd.Process.Pid, stdout)
	defer stopWatcher()

	sessionID := opts.SessionID
	var model string
	var textParts []string
	var usage ModelUsageInfo
	var rateLimit *RateLimitInfo

	// sawText tracks whether the stream emitted any assistant text block for
	// this turn. When set, we ignore the redundant `result.text` field (which
	// Claude echoes from the final assistant block) to avoid duplicating the
	// tail of the answer in RunResult.Text.
	var sawText bool

	// pending accumulates consecutive assistant text blocks until a
	// chronological boundary arrives — a tool invocation, an unknown
	// progress item, a rate-limit event, or end of stream. On a boundary
	// the accumulated block is demoted to a single narration progress
	// event (preserving order in the log); at end of stream it is
	// promoted to the final answer body. Accumulating across raw block
	// boundaries keeps sequential text fragments of one logical reply
	// together instead of artificially splitting them.
	var pending string
	demotePending := func() {
		if pending == "" {
			return
		}
		if onProgress != nil {
			onProgress(ProgressEvent{Kind: ProgressKindNarration, Text: pending})
		}
		pending = ""
	}
	appendPending := func(t string) {
		t = strings.Trim(t, "\n")
		if t == "" {
			return
		}
		if pending != "" {
			pending += "\n\n" + t
		} else {
			pending = t
		}
		sawText = true
	}

	// done flips on `result` so later text/tool events in the stream
	// cannot overwrite the locked-in final answer. Protects the
	// invariant "once result arrives, the turn is over".
	var done bool

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if done {
			// Drain further output to keep the pipe unblocked but do
			// not touch state: the turn is finalised.
			continue
		}

		events, ok := backend.ParseEvent(line)
		if !ok {
			continue
		}

		for _, ev := range events {
			switch ev.Type {
			case "system":
				if ev.SessionID != "" {
					sessionID = ev.SessionID
				}
				if ev.Model != "" {
					model = ev.Model
				}
				if ev.RateLimit != nil {
					rateLimit = ev.RateLimit
					if onProgress != nil && ev.RateLimit.Status != "allowed" {
						// Flush pending first so the rate-limit line
						// appears in the log AFTER the text that preceded
						// it chronologically.
						demotePending()
						onProgress(ProgressEvent{Kind: ProgressKindTool, Text: formatRateLimit(ev.RateLimit)})
					}
				}

			case "tool":
				r.mu.Lock()
				r.current = ev.Tool
				r.toolAt = time.Now()
				r.mu.Unlock()
				// A tool call is the chronological boundary between
				// narration and what comes after. Flush pending BEFORE
				// emitting the tool so the log order matches the stream.
				demotePending()
				if onProgress != nil {
					onProgress(ProgressEvent{Kind: ProgressKindTool, Text: ev.Tool.String()})
				}
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}

			case "text":
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}
				appendPending(ev.Text)

			case "intermediate":
				// Codex emits one `agent_message` per assistant text
				// fragment within a turn. Accumulate; the boundary is
				// the next tool/end-of-stream, same as Claude.
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				appendPending(ev.Text)

			case "unknown":
				if onProgress != nil && ev.Text != "" {
					demotePending()
					onProgress(ProgressEvent{Kind: ProgressKindTool, Text: fmt.Sprintf("❓ %s", ev.Text)})
				}

			case "result":
				done = true
				if ev.Error != "" {
					// Preserve any accumulated text as narration so the
					// user still sees what the model said before the
					// error — the queue error branch renders logItems
					// alongside the error marker.
					demotePending()
					return RunResult{
						SessionID: sessionID,
						Error:     fmt.Errorf("%s: %s", backend.Name(), ev.Error),
					}
				}
				// Trust result.text only when no assistant text blocks
				// were seen (older Claude streams, or backends that do
				// not emit per-block text). Otherwise result.text just
				// duplicates the block already held in pending.
				if ev.Text != "" && !sawText {
					textParts = append(textParts, ev.Text)
				}
				// Merge usage from result event.
				if ev.Usage.Model != "" {
					usage.Model = ev.Usage.Model
				}
				if ev.Usage.ContextWindow > 0 {
					usage.ContextWindow = ev.Usage.ContextWindow
				}
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.CacheRead > 0 {
					usage.CacheRead = ev.Usage.CacheRead
				}
				if ev.Usage.CacheCreation > 0 {
					usage.CacheCreation = ev.Usage.CacheCreation
				}
			}
		}
	}

	waitErr := cmd.Wait()

	// Cancellation path: whatever is in pending is not a sanctioned
	// final answer, but it may be substantial narration the user was
	// about to see. Demote it so the error branch in queue.go can surface
	// it via logItems alongside the error marker. `Error != nil` still
	// signals the caller that the turn did not complete, so session
	// counters / model state do not advance.
	if ctx.Err() != nil {
		demotePending()
		return RunResult{
			SessionID: sessionID,
			Error:     fmt.Errorf("%s: %w", backend.Name(), ctx.Err()),
		}
	}

	// The run completed normally — whatever stayed in pending past the
	// last tool boundary is the actual reply.
	if pending != "" {
		textParts = append(textParts, pending)
		pending = ""
	}

	if usage.Model == "" {
		usage.Model = model
	}

	// For codex: read model, effective context window, and the latest turn's
	// prompt size from the local session file. On resumed runs, codex may not
	// re-emit thread.started, so fall back to the SessionID we already passed.
	if backend.Name() == "codex" && sessionID != "" {
		if m, cw, cu := ReadCodexSessionMeta(sessionID); m != "" || cw > 0 || cu > 0 {
			if usage.Model == "" {
				usage.Model = m
			}
			if usage.ContextWindow == 0 {
				usage.ContextWindow = cw
			}
			if cu > 0 {
				usage.ContextUsed = cu
			}
		}
	}

	text := strings.Join(textParts, "\n")
	result := RunResult{
		SessionID: sessionID,
		Text:      text,
		Usage:     usage,
		RateLimit: rateLimit,
	}
	if text == "" && waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			result.Error = fmt.Errorf("%s: %s", backend.Name(), stderr)
		} else {
			result.Error = fmt.Errorf("%s exited: %w", backend.Name(), waitErr)
		}
	}
	return result
}

// watchCancel escalates ctx cancellation into process-group termination. The
// backend command is launched with Setpgid, so the child's pid equals the
// pgid and we can signal every descendant (e.g. the rust grandchild behind
// the codex npm shim) with a single Kill(-pid).
//
// On cancel it sends SIGTERM, waits killGracePeriod, then SIGKILL and closes
// stdout so the scanner loop unblocks even if a grandchild still holds a
// write-end of the pipe.
//
// The returned stop function must be called when the Run completes normally
// to release the watcher goroutine.
func watchCancel(ctx context.Context, pid int, stdout io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		// Signal the entire process group. Negative pid in Kill targets the
		// group whose leader has |pid|. Errors here are best-effort: the
		// process may have already exited.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(killGracePeriod):
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		// Closing stdout unblocks the scanner if a grandchild inherited and
		// still holds the write-end after the shim exited.
		if stdout != nil {
			if err := stdout.Close(); err != nil {
				log.Printf("runner: stdout close after cancel failed: %v", err)
			}
		}
	}()
	return func() { close(done) }
}
