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
	"unicode/utf8"

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
	if n <= 0 {
		if s == "" {
			return s
		}
		return "…"
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
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

// RunResult is the final result of a CLI invocation. The reply body itself is
// not carried here — assistant text streams out through `onProgress` as it
// arrives (one ProgressKindNarration per content block), so when Run returns
// the chat already shows everything the model said. RunResult only carries
// what the frontend needs *after* the run: session ID for persistence, usage
// counters for the status line, rate-limit snapshot, and a terminal error.
type RunResult struct {
	SessionID string
	Usage     ModelUsageInfo
	RateLimit *RateLimitInfo
	Error     error
}

// ProgressKind discriminates progress events surfaced during a run.
type ProgressKind string

const (
	// ProgressKindTool is a tool invocation label ("⚙️ Bash: ls ~").
	ProgressKindTool ProgressKind = "tool"
	// ProgressKindNarration is an assistant text block streamed during the
	// run. Frontends render these blocks directly in the live run log.
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
	var usage ModelUsageInfo
	var rateLimit *RateLimitInfo

	// sawText records whether any assistant text/intermediate block was
	// streamed out. Used to decide at end-of-run whether a non-zero exit
	// from the backend should be surfaced as an error: if the model did
	// produce a reply (already visible in the chat) we treat the exit as
	// cosmetic; only a truly silent failure becomes RunResult.Error.
	var sawText bool

	// emitText streams an assistant text/intermediate block out as
	// narration immediately. No buffering, no waiting for a boundary —
	// the frontend appends it to the live progress message as soon as
	// it arrives. Empty/whitespace-only blocks are dropped.
	emitText := func(t string) {
		t = strings.Trim(t, "\n")
		if t == "" {
			return
		}
		sawText = true
		if onProgress == nil {
			return
		}
		onProgress(ProgressEvent{Kind: ProgressKindNarration, Text: t})
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
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
						onProgress(ProgressEvent{Kind: ProgressKindTool, Text: formatRateLimit(ev.RateLimit)})
					}
				}

			case "tool":
				r.mu.Lock()
				r.current = ev.Tool
				r.toolAt = time.Now()
				r.mu.Unlock()
				if onProgress != nil {
					onProgress(ProgressEvent{Kind: ProgressKindTool, Text: ev.Tool.String()})
				}
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}

			case "text", "intermediate":
				// Stream each assistant text block (Claude) or agent_message
				// fragment (codex) as it arrives. There is no "is this the
				// final answer?" guess any more — every block is shown live,
				// and the run's natural end (EOF, normal exit) is the
				// finality signal for the frontend.
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}
				emitText(ev.Text)

			case "unknown":
				if onProgress != nil && ev.Text != "" {
					onProgress(ProgressEvent{Kind: ProgressKindTool, Text: fmt.Sprintf("❓ %s", ev.Text)})
				}

			case "result":
				// `result` marks the end of one agent-loop iteration, not
				// the end of the run. claude -p --output-format stream-json
				// keeps the loop alive while background tasks (run_in_background,
				// Monitor) are pending and injects their completions as
				// <task-notification> user messages → another assistant turn
				// → another `result`. We let those subsequent events flow
				// through normally; the run ends when the CLI exits and the
				// scanner hits EOF.
				if ev.Error != "" {
					return RunResult{
						SessionID: sessionID,
						Error:     fmt.Errorf("%s: %s", backend.Name(), ev.Error),
					}
				}
				// result.text is intentionally ignored: assistant content was
				// already streamed block-by-block via emitText. Keeping it
				// would duplicate the tail of the reply in the chat.
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

	if ctx.Err() != nil {
		return RunResult{
			SessionID: sessionID,
			Error:     fmt.Errorf("%s: %w", backend.Name(), ctx.Err()),
		}
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

	result := RunResult{
		SessionID: sessionID,
		Usage:     usage,
		RateLimit: rateLimit,
	}
	if waitErr != nil && !sawText {
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
