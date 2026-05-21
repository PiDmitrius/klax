package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ClaudeBackend implements Backend for Claude Code CLI.
type ClaudeBackend struct{}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) BuildCmd(opts RunOptions) (*exec.Cmd, error) {
	var mode string
	if opts.Sandbox == "" || opts.Sandbox == "off" {
		mode = "bypassPermissions"
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		// Agent: sub-agent spawn — klax tracks one process per session.
		// AskUserQuestion: needs a TTY to render its TUI; in `claude -p` it
		// silently fails, returns no answer, and the turn ends with an empty
		// "🔧 AskUserQuestion ✅ Готово" stub in chat.
		"--disallowed-tools", "Agent,AskUserQuestion",
	}
	if mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}

	bin := findBinary("claude", []string{".local/bin/claude"})
	if bin == "" {
		return nil, errors.New("claude not found. Install: curl -fsSL https://claude.ai/install.sh | bash")
	}

	cmd := exec.Command(bin, args...)
	if opts.CWD != "" {
		cmd.Dir = opts.CWD
	}
	cmd.Stdin = strings.NewReader(opts.Prompt)
	// Own process group so any grandchildren (plugins, subshells) can be
	// signalled together via syscall.Kill(-pgid, ...).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// claudeStreamEvent is the raw JSON from claude --output-format stream-json.
type claudeStreamEvent struct {
	Type          string                     `json:"type"`
	Name          string                     `json:"name,omitempty"`
	Input         json.RawMessage            `json:"input,omitempty"`
	Result        string                     `json:"result,omitempty"`
	IsError       bool                       `json:"is_error,omitempty"`
	SessionID     string                     `json:"session_id,omitempty"`
	Model         string                     `json:"model,omitempty"`
	ModelUsage    map[string]json.RawMessage `json:"modelUsage,omitempty"`
	Message       *claudeMessage             `json:"message,omitempty"`
	RateLimitInfo *claudeRateLimitInfo       `json:"rate_limit_info,omitempty"`
}

type claudeRateLimitInfo struct {
	Status         string  `json:"status"`        // "allowed" | "allowed_warning" | "throttled" | "rejected"
	ResetsAt       int64   `json:"resetsAt"`      // unix timestamp
	RateLimitType  string  `json:"rateLimitType"` // "five_hour" | "seven_day"
	Utilization    float64 `json:"utilization"`   // 0.0–1.0
	OverageStatus  string  `json:"overageStatus"` // "allowed" | ...
	IsUsingOverage bool    `json:"isUsingOverage"`
}

type claudeMessage struct {
	Content []claudeContentBlock `json:"content"`
	Usage   *claudeMessageUsage  `json:"usage,omitempty"`
}

type claudeMessageUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (b *ClaudeBackend) ParseEvent(line []byte) ([]Event, bool) {
	var ev claudeStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, false
	}

	switch ev.Type {
	case "system":
		return []Event{{
			Type:      "system",
			SessionID: ev.SessionID,
			Model:     ev.Model,
		}}, true

	case "user":
		return nil, false

	case "rate_limit_event":
		if ev.RateLimitInfo != nil {
			return []Event{{
				Type: "system",
				RateLimit: &RateLimitInfo{
					Status:         ev.RateLimitInfo.Status,
					ResetsAt:       ev.RateLimitInfo.ResetsAt,
					RateLimitType:  ev.RateLimitInfo.RateLimitType,
					Utilization:    ev.RateLimitInfo.Utilization,
					IsUsingOverage: ev.RateLimitInfo.IsUsingOverage,
				},
			}}, true
		}
		return nil, false

	case "assistant":
		if ev.Message == nil {
			return nil, false
		}
		// Track context usage from message; stamp it on the first emitted
		// event so the runner can track it without double-counting.
		var usage ModelUsageInfo
		if u := ev.Message.Usage; u != nil {
			usage.ContextUsed = u.InputTokens + u.CacheRead + u.CacheCreation
		}
		var out []Event
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "tool_use":
				name := block.Name
				input := string(block.Input)
				if name == "TodoWrite" {
					name = "Plan"
					input = claudePlanInput(block.Input)
				}
				e := Event{
					Type: "tool",
					Tool: ToolUse{Name: name, Input: input},
				}
				if len(out) == 0 {
					e.Usage = usage
				}
				out = append(out, e)
			case "text":
				e := Event{Type: "text", Text: block.Text}
				if len(out) == 0 {
					e.Usage = usage
				}
				out = append(out, e)
			}
			// Other block types (`thinking`, `redacted_thinking`, `image`) are
			// intentionally dropped — extended-thinking blocks would flood the
			// chat with raw chain-of-thought, and we already surface tool calls
			// + final text. Promote here if a future product decision wants to
			// expose them.
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true

	case "result":
		var e Event
		e.Type = "result"
		if ev.IsError && ev.Result != "" {
			e.Error = ev.Result
		} else if ev.Result != "" {
			e.Text = ev.Result
		}
		// Pick the model with the most output tokens (primary model).
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
				e.Usage.Model = modelName
				e.Usage.ContextWindow = mu.ContextWindow
				e.Usage.InputTokens = mu.InputTokens
				e.Usage.OutputTokens = mu.OutputTokens
				e.Usage.CacheRead = mu.CacheReadInputTokens
				e.Usage.CacheCreation = mu.CacheCreationTokens
			}
		}
		return []Event{e}, true
	}

	return []Event{{Type: "unknown", Text: ev.Type}}, true
}

// claudePlanInput normalizes Claude's TodoWrite tool_use input
// ({"todos":[{"content","status","activeForm"}]}) into the canonical
// PlanProgress JSON. "current" prefers the activeForm of the in_progress
// item, falling back to the content of the first pending one.
func claudePlanInput(raw json.RawMessage) string {
	var parsed struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"` // pending | in_progress | completed
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Todos) == 0 {
		return string(raw)
	}
	done := 0
	current := ""
	for _, td := range parsed.Todos {
		if td.Status == "completed" {
			done++
		}
	}
	for _, td := range parsed.Todos {
		if td.Status == "in_progress" {
			if td.ActiveForm != "" {
				current = td.ActiveForm
			} else {
				current = td.Content
			}
			break
		}
	}
	if current == "" {
		for _, td := range parsed.Todos {
			if td.Status == "pending" {
				current = td.Content
				break
			}
		}
	}
	return MarshalPlanProgress(done, len(parsed.Todos), current)
}

// findBinary looks for a binary by name, with fallback paths relative to $HOME.
func findBinary(name string, homePaths []string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		for _, rel := range homePaths {
			candidate := filepath.Join(home, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	// Also check /usr/local/bin.
	candidate := fmt.Sprintf("/usr/local/bin/%s", name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
