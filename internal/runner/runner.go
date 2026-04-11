// Package runner executes AI CLI tools and streams results.
package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ToolUse describes what the AI is doing right now.
type ToolUse struct {
	Name  string
	Input string
}

func (t ToolUse) String() string {
	switch t.Name {
	case "Bash":
		var inp struct{ Command string }
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("⚙️ Bash: `%s`", truncate(inp.Command, 60))
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
		return fmt.Sprintf("🌐 Fetch: %s", truncate(inp.URL, 60))
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
	default:
		return fmt.Sprintf("🔧 %s", t.Name)
	}
}

func truncate(s string, n int) string {
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
	PermissionMode     string // claude: acceptEdits | bypassPermissions | auto
	Model              string // model override
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
	Error     error
}

// ProgressFunc is called with human-readable progress updates.
type ProgressFunc func(status string)

// Runner executes an AI backend and tracks state.
type Runner struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
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

// Abort kills the current process.
func (r *Runner) Abort() {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// Run executes the backend with streaming output.
func (r *Runner) Run(backend Backend, opts RunOptions, onProgress ProgressFunc) RunResult {
	r.mu.Lock()
	r.busy = true
	r.startAt = time.Now()
	r.current = ToolUse{}
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.busy = false
		r.cmd = nil
		r.current = ToolUse{}
		r.mu.Unlock()
	}()

	cmd, err := backend.BuildCmd(opts)
	if err != nil {
		return RunResult{Error: err}
	}

	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{Error: fmt.Errorf("pipe: %w", err)}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return RunResult{Error: fmt.Errorf("start: %w", err)}
	}

	var sessionID string
	var model string
	var textParts []string
	var lastIntermediate string // last intermediate message (codex thinking)
	var usage ModelUsageInfo

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		ev, ok := backend.ParseEvent(line)
		if !ok {
			continue
		}

		switch ev.Type {
		case "system":
			if ev.SessionID != "" {
				sessionID = ev.SessionID
			}
			if ev.Model != "" {
				model = ev.Model
			}

		case "tool":
			r.mu.Lock()
			r.current = ev.Tool
			r.toolAt = time.Now()
			r.mu.Unlock()
			if onProgress != nil {
				onProgress(ev.Tool.String())
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

		case "intermediate":
			// Codex intermediate "thinking" message — overwrite previous,
			// only the last one becomes the final answer.
			lastIntermediate = ev.Text
			r.mu.Lock()
			r.current = ToolUse{}
			r.mu.Unlock()

		case "result":
			if ev.Error != "" {
				return RunResult{
					SessionID: sessionID,
					Error:     fmt.Errorf("%s: %s", backend.Name(), ev.Error),
				}
			}
			if ev.Text != "" {
				textParts = append(textParts, ev.Text)
			}
			// Merge usage from result event.
			if ev.Usage.Model != "" {
				usage.Model = ev.Usage.Model
			}
			if ev.Usage.ContextWindow > 0 {
				usage.ContextWindow = ev.Usage.ContextWindow
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

	// For codex: if no explicit result text, use last intermediate message.
	if len(textParts) == 0 && lastIntermediate != "" {
		textParts = append(textParts, lastIntermediate)
	}

	waitErr := cmd.Wait()

	if usage.Model == "" {
		usage.Model = model
	}

	text := strings.Join(textParts, "\n")
	result := RunResult{
		SessionID: sessionID,
		Text:      text,
		Usage:     usage,
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
