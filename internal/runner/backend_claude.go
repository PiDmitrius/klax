package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ClaudeBackend implements Backend for Claude Code CLI.
type ClaudeBackend struct {
	PermissionMode string
}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) BuildCmd(opts RunOptions) (*exec.Cmd, error) {
	mode := opts.PermissionMode
	if mode == "" {
		mode = b.PermissionMode
	}
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

	bin := findBinary("claude", []string{".local/bin/claude"})
	if bin == "" {
		return nil, errors.New("claude not found. Install: curl -fsSL https://claude.ai/install.sh | bash")
	}

	cmd := exec.Command(bin, args...)
	if opts.CWD != "" {
		cmd.Dir = opts.CWD
	}
	cmd.Stdin = strings.NewReader(opts.Prompt)
	return cmd, nil
}

// claudeStreamEvent is the raw JSON from claude --output-format stream-json.
type claudeStreamEvent struct {
	Type       string                     `json:"type"`
	Name       string                     `json:"name,omitempty"`
	Input      json.RawMessage            `json:"input,omitempty"`
	Result     string                     `json:"result,omitempty"`
	IsError    bool                       `json:"is_error,omitempty"`
	SessionID  string                     `json:"session_id,omitempty"`
	Model      string                     `json:"model,omitempty"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage,omitempty"`
	Message    *claudeMessage             `json:"message,omitempty"`
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

func (b *ClaudeBackend) ParseEvent(line []byte) (Event, bool) {
	var ev claudeStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, false
	}

	switch ev.Type {
	case "system":
		return Event{
			Type:      "system",
			SessionID: ev.SessionID,
			Model:     ev.Model,
		}, true

	case "user", "rate_limit_event":
		return Event{}, false

	case "assistant":
		if ev.Message == nil {
			return Event{}, false
		}
		// Track context usage from message.
		var usage ModelUsageInfo
		if u := ev.Message.Usage; u != nil {
			usage.ContextUsed = u.InputTokens + u.CacheRead + u.CacheCreation
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "tool_use":
				return Event{
					Type:  "tool",
					Tool:  ToolUse{Name: block.Name, Input: string(block.Input)},
					Usage: usage,
				}, true
			case "text":
				return Event{Type: "text", Usage: usage}, true
			}
		}
		return Event{}, false

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
		return e, true
	}

	return Event{Type: "unknown", Text: ev.Type}, true
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
