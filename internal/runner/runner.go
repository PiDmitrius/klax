// Package runner executes claude CLI and streams results.
package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// StreamEvent is a parsed event from claude --output-format stream-json
type StreamEvent struct {
	Type       string                     `json:"type"`
	Name       string                     `json:"name,omitempty"`
	Input      json.RawMessage            `json:"input,omitempty"`
	Result     string                     `json:"result,omitempty"`
	IsError    bool                       `json:"is_error,omitempty"`
	Subtype    string                     `json:"subtype,omitempty"`
	Message    *Message                   `json:"message,omitempty"`
	SessionID  string                     `json:"session_id,omitempty"`
	Model      string                     `json:"model,omitempty"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage,omitempty"`
}

type Message struct {
	Content []ContentBlock `json:"content"`
	Usage   *MessageUsage  `json:"usage,omitempty"`
}

type MessageUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolUse describes what Claude is doing right now.
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

// RunOptions configures a claude invocation.
type RunOptions struct {
	Prompt             string
	SessionID          string // empty = new session
	CWD                string // working directory
	PermissionMode     string // acceptEdits | bypassPermissions | auto | default
	Model              string // claude model override
	AppendSystemPrompt string // appended to default system prompt
}

// ModelUsageInfo captures context window usage from a claude run.
type ModelUsageInfo struct {
	Model         string
	ContextWindow int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreation int
	ContextUsed   int // input_tokens + cache_read + cache_creation from last assistant msg
}

// RunResult is the final result of a claude invocation.
type RunResult struct {
	SessionID string
	Text      string
	Usage     ModelUsageInfo
	Error     error
}

// ProgressFunc is called with human-readable progress updates.
type ProgressFunc func(status string)

// Runner executes claude and tracks state.
type Runner struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	busy    bool
	current ToolUse
	startAt time.Time // when Run() started
	toolAt  time.Time // when current tool_use started
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

// Abort kills the current claude process.
func (r *Runner) Abort() {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// Run executes claude with streaming output.
// onProgress is called when tool use changes.
// Returns final result.
func (r *Runner) Run(opts RunOptions, onProgress ProgressFunc) RunResult {
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

	mode := opts.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
		"--disallowed-tools", "Agent",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	// Prompt is passed via stdin, not as a CLI argument.
	// This avoids arg length limits, hides content from `ps`, and prevents
	// any interpretation of user text as flags.

	claudeBin := "claude"
	if p, err := exec.LookPath("claude"); err == nil {
		claudeBin = p
	} else {
		// Fallback: common install locations when PATH is stripped (e.g. systemd)
		home, _ := os.UserHomeDir()
		candidates := []string{"/usr/local/bin/claude"}
		if home != "" {
			candidates = append([]string{filepath.Join(home, ".local", "bin", "claude")}, candidates...)
		}
		for _, candidate := range candidates {
			if _, err := exec.LookPath(candidate); err == nil {
				claudeBin = candidate
				break
			}
		}
	}
	cmd := exec.Command(claudeBin, args...)
	if opts.CWD != "" {
		cmd.Dir = opts.CWD
	}
	cmd.Stdin = strings.NewReader(opts.Prompt)

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
		if errors.Is(err, exec.ErrNotFound) {
			return RunResult{Error: fmt.Errorf("claude not found. Install: npm install -g @anthropic-ai/claude-code")}
		}
		return RunResult{Error: fmt.Errorf("start: %w", err)}
	}

	var sessionID string
	var model string
	var textParts []string
	var usage ModelUsageInfo
	var streamError string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
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

		case "assistant":
			if ev.Message == nil {
				continue
			}
			if u := ev.Message.Usage; u != nil {
				usage.ContextUsed = u.InputTokens + u.CacheRead + u.CacheCreation
			}
			for _, block := range ev.Message.Content {
				switch block.Type {
				case "tool_use":
					tu := ToolUse{Name: block.Name, Input: string(block.Input)}
					r.mu.Lock()
					r.current = tu
					r.toolAt = time.Now()
					r.mu.Unlock()
					if onProgress != nil {
						onProgress(tu.String())
					}
				case "text":
					// Claude is writing text — clear stale tool status.
					r.mu.Lock()
					r.current = ToolUse{}
					r.mu.Unlock()
				}
			}

		case "result":
			if ev.IsError && ev.Result != "" {
				streamError = ev.Result
			} else if ev.Result != "" {
				textParts = append(textParts, ev.Result)
			}
			// Extract model usage from result event.
			// Pick the model with the most output tokens (primary model),
			// not background models (haiku for compaction etc).
			bestOutput := -1
			for modelName, raw := range ev.ModelUsage {
				var mu struct {
					InputTokens          int `json:"inputTokens"`
					OutputTokens         int `json:"outputTokens"`
					CacheReadInputTokens int `json:"cacheReadInputTokens"`
					CacheCreationTokens  int `json:"cacheCreationInputTokens"`
					ContextWindow        int `json:"contextWindow"`
				}
				if json.Unmarshal(raw, &mu) == nil && mu.OutputTokens > bestOutput {
					bestOutput = mu.OutputTokens
					usage.Model = modelName
					usage.ContextWindow = mu.ContextWindow
					usage.InputTokens = mu.InputTokens
					usage.OutputTokens = mu.OutputTokens
					usage.CacheRead = mu.CacheReadInputTokens
					usage.CacheCreation = mu.CacheCreationTokens
				}
			}
		}
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
	if streamError != "" {
		result.Error = fmt.Errorf("claude: %s", streamError)
	} else if text == "" && waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			result.Error = fmt.Errorf("claude: %s", stderr)
		} else {
			result.Error = fmt.Errorf("claude exited: %w", waitErr)
		}
	}
	return result
}
