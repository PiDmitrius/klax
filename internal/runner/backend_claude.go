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
//
// Per-run parser state (reset in BuildCmd and on message_start):
//   - partialDeltaSeen: true once a text-delta has arrived. Used to skip
//     the assistant event's text blocks under --include-partial-messages
//     because they duplicate what was already streamed via deltas.
//   - inTextBlock: true while text deltas are arriving for the current
//     content block. Used to emit EventTextBoundary on the matching
//     content_block_stop so the runner separates back-to-back text
//     blocks with a paragraph break.
type ClaudeBackend struct {
	partialDeltaSeen bool
	inTextBlock      bool
}

func (b *ClaudeBackend) Name() string { return "claude" }

// BuildClaudeArgs assembles the claude CLI flag list for a run, independent of
// binary resolution and the klax-tty wrapping done in BuildCmd. Kept separate
// so the tty arg parser can be coupled to the real flag set in a test without
// the claude binary being installed.
func BuildClaudeArgs(opts RunOptions) []string {
	var mode string
	if opts.Sandbox == "" || opts.Sandbox == "off" {
		mode = "bypassPermissions"
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		// Stream individual text deltas as `stream_event content_block_delta
		// text_delta` lines in addition to the final `assistant` event.
		// Without this flag claude buffers the whole assistant message until
		// the API call completes, so the runner sees zero output for the
		// entire turn and then the full reply in one chunk — defeating any
		// look-ahead streaming downstream. ParseEvent consumes the deltas and
		// skips the redundant text blocks in the assistant event to avoid
		// double-counting.
		"--include-partial-messages",
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
	return args
}

func (b *ClaudeBackend) BuildCmd(opts RunOptions) (*exec.Cmd, error) {
	// Reset per-run parser state on entry so a reused backend instance
	// doesn't carry block-tracking from a prior turn.
	b.partialDeltaSeen = false
	b.inTextBlock = false
	args := BuildClaudeArgs(opts)

	bin := findBinary("claude", []string{".local/bin/claude"})
	if bin == "" {
		return nil, errors.New("claude not found. Install: curl -fsSL https://claude.ai/install.sh | bash")
	}
	if opts.ClaudeTTY {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate klax executable: %w", err)
		}
		args = append([]string{"tty", bin}, args...)
		bin = self
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
	Type            string                     `json:"type"`
	Subtype         string                     `json:"subtype,omitempty"`
	Name            string                     `json:"name,omitempty"`
	Input           json.RawMessage            `json:"input,omitempty"`
	Result          string                     `json:"result,omitempty"`
	IsError         bool                       `json:"is_error,omitempty"`
	SessionID       string                     `json:"session_id,omitempty"`
	Model           string                     `json:"model,omitempty"`
	ModelUsage      map[string]json.RawMessage `json:"modelUsage,omitempty"`
	Message         *claudeMessage             `json:"message,omitempty"`
	RateLimitInfo   *claudeRateLimitInfo       `json:"rate_limit_info,omitempty"`
	Event           *claudeNestedEvent         `json:"event,omitempty"`
	CompactMetadata *claudeCompactMetadata     `json:"compactMetadata,omitempty"`
}

// claudeCompactMetadata is the token-delta payload of a compact_boundary line.
type claudeCompactMetadata struct {
	Trigger    string `json:"trigger"`
	PreTokens  int    `json:"preTokens"`
	PostTokens int    `json:"postTokens"`
}

// claudeNestedEvent is the inner payload of a `stream_event` outer envelope.
// We only need the text-delta path; the other variants (message_start,
// content_block_start, content_block_stop, message_delta, message_stop,
// input_json_delta) carry information we already derive from the higher-
// level assistant and result events.
type claudeNestedEvent struct {
	Type  string                   `json:"type"`
	Delta *claudeContentBlockDelta `json:"delta,omitempty"`
}

type claudeContentBlockDelta struct {
	// Type is "text_delta" for assistant text or "input_json_delta" for the
	// partial JSON of a tool input. We only surface text_delta — tool inputs
	// are read off the completed `assistant` event so we never have to glue
	// a partial JSON fragment back together.
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
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
		if ev.Subtype == "compact_boundary" && ev.CompactMetadata != nil {
			return []Event{{
				Type: EventCompact,
				Compact: &CompactInfo{
					Trigger:    ev.CompactMetadata.Trigger,
					PreTokens:  ev.CompactMetadata.PreTokens,
					PostTokens: ev.CompactMetadata.PostTokens,
				},
			}}, true
		}
		return []Event{{
			Type:      EventSystem,
			SessionID: ev.SessionID,
			Model:     ev.Model,
		}}, true

	case "user":
		return nil, false

	case "rate_limit_event":
		if ev.RateLimitInfo != nil {
			return []Event{{
				Type: EventSystem,
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

	case "stream_event":
		// We consume three inner events:
		//   - content_block_delta with text_delta is the streaming surface.
		//   - message_start resets per-turn parser state. claude -p
		//     produces multiple assistant messages in one Run; resetting
		//     protects a turn that genuinely lacks deltas from being
		//     treated as partial-mode by stale state from a prior turn.
		//   - content_block_stop on a text block emits a boundary so the
		//     runner inserts a paragraph break between adjacent blocks.
		//
		// Other inner events (content_block_start, input_json_delta,
		// message_delta, message_stop) are no-ops — usage and tool calls
		// come from the higher-level assistant and result events.
		if ev.Event == nil {
			return nil, false
		}
		switch ev.Event.Type {
		case "message_start":
			b.partialDeltaSeen = false
			b.inTextBlock = false
			return nil, false
		case "content_block_delta":
			d := ev.Event.Delta
			if d == nil || d.Type != "text_delta" || d.Text == "" {
				return nil, false
			}
			b.partialDeltaSeen = true
			b.inTextBlock = true
			return []Event{{Type: EventTextDelta, Text: d.Text}}, true
		case "content_block_stop":
			// End of a text block: emit a boundary so the runner inserts
			// the paragraph break the legacy paragraph-join would have
			// produced. Skip if the block was not text.
			if !b.inTextBlock {
				return nil, false
			}
			b.inTextBlock = false
			return []Event{{Type: EventTextBoundary}}, true
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
					Type: EventTool,
					Tool: ToolUse{Name: name, Input: input},
				}
				if len(out) == 0 {
					e.Usage = usage
				}
				out = append(out, e)
			case "text":
				// Under --include-partial-messages this block mirrors the
				// text we already streamed via deltas; skip it. Without
				// the flag, partialDeltaSeen stays false and we emit
				// normally.
				if b.partialDeltaSeen {
					continue
				}
				e := Event{Type: EventText, Text: block.Text}
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
		e.Type = EventResult
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

	return []Event{{Type: EventUnknown, Text: ev.Type}}, true
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
