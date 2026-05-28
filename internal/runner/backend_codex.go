package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CodexBackend implements Backend for OpenAI Codex CLI.
type CodexBackend struct{}

func (b *CodexBackend) Name() string { return "codex" }

func (b *CodexBackend) BuildCmd(opts RunOptions) (*exec.Cmd, error) {
	var args []string

	if opts.SessionID != "" {
		// Resume existing session.
		args = []string{"exec", "resume", opts.SessionID, "--json", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check"}
	}

	if opts.Sandbox == "" || opts.Sandbox == "off" {
		if opts.SessionID != "" {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "--sandbox", "danger-full-access")
		}
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("reasoning_effort=%q", opts.Effort))
	}

	// Prompt via stdin.
	args = append(args, "-")

	bin := findBinary("codex", []string{".npm-global/bin/codex"})
	if bin == "" {
		return nil, errors.New("codex not found. Install: npm install -g @openai/codex")
	}

	cmd := exec.Command(bin, args...)
	if opts.CWD != "" {
		cmd.Dir = opts.CWD
	}
	cmd.Stdin = strings.NewReader(opts.Prompt)
	// Own process group so grandchildren (the npm shim spawns the real rust
	// binary) can be signalled together via syscall.Kill(-pgid, ...).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// codexEvent is the raw JSON from codex exec --json.
type codexEvent struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id,omitempty"`
	Item     *codexItem  `json:"item,omitempty"`
	Usage    *codexUsage `json:"usage,omitempty"`
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"` // agent_message | command_execution | web_search | file_read | file_edit | file_change | mcp_tool_call | ...
	Text             string          `json:"text,omitempty"`
	Message          json.RawMessage `json:"message,omitempty"`
	Error            json.RawMessage `json:"error,omitempty"`
	Command          string          `json:"command,omitempty"`
	AggregatedOutput string          `json:"aggregated_output,omitempty"`
	ExitCode         *int            `json:"exit_code,omitempty"`
	Status           string          `json:"status,omitempty"`
	Query            string          `json:"query,omitempty"`     // web_search
	FilePath         string          `json:"file_path,omitempty"` // file_read, file_edit
	Changes          []codexChange   `json:"changes,omitempty"`   // file_change
	Action           json.RawMessage `json:"action,omitempty"`
	Server           string          `json:"server,omitempty"` // mcp_tool_call
	Tool             string          `json:"tool,omitempty"`   // mcp_tool_call
	Items            []codexPlanItem `json:"items,omitempty"`  // todo_list
}

type codexPlanItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type codexChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // add, update, delete
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

func (b *CodexBackend) ParseEvent(line []byte) ([]Event, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, false
	}

	single := func(e Event) ([]Event, bool) { return []Event{e}, true }

	switch ev.Type {
	case "thread.started":
		return single(Event{
			Type:      EventSystem,
			SessionID: ev.ThreadID,
		})

	case "item.started":
		if ev.Item == nil {
			return nil, false
		}
		switch ev.Item.Type {
		case "command_execution":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{
					Name:  "Bash",
					Input: fmt.Sprintf(`{"command":"%s"}`, escapeJSON(ev.Item.Command)),
				},
			})
		case "web_search":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{Name: "WebSearch", Input: ""},
			})
		case "file_read":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{
					Name:  "Read",
					Input: fmt.Sprintf(`{"file_path":"%s"}`, escapeJSON(ev.Item.FilePath)),
				},
			})
		case "file_edit":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{
					Name:  "Edit",
					Input: fmt.Sprintf(`{"file_path":"%s"}`, escapeJSON(ev.Item.FilePath)),
				},
			})
		case "file_change":
			name := "Edit"
			if len(ev.Item.Changes) == 1 && ev.Item.Changes[0].Kind == "add" {
				name = "Write"
			}
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{
					Name:  name,
					Input: fmt.Sprintf(`{"file_path":"%s"}`, escapeJSON(codexChangePaths(ev.Item.Changes))),
				},
			})
		case "todo_list":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{Name: "Plan", Input: codexPlanInput(ev.Item.Items)},
			})
		case "mcp_tool_call":
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{
					Name:  "MCP",
					Input: fmt.Sprintf(`{"server":"%s","tool":"%s"}`, escapeJSON(ev.Item.Server), escapeJSON(ev.Item.Tool)),
				},
			})
		case "error":
			return single(Event{Type: EventError, Text: codexErrorItemText(ev.Item)})
		}
		return single(Event{Type: EventUnknown, Text: fmt.Sprintf("item.started:%s", ev.Item.Type)})

	case "item.completed":
		if ev.Item == nil {
			return nil, false
		}
		switch ev.Item.Type {
		case "agent_message":
			return single(Event{
				Type: EventIntermediate,
				Text: ev.Item.Text,
			})
		case "command_execution":
			return single(Event{Type: EventText})
		case "web_search":
			query := ev.Item.Query
			if query != "" {
				return single(Event{
					Type: EventTool,
					Tool: ToolUse{
						Name:  "WebSearch",
						Input: fmt.Sprintf(`{"query":"%s"}`, escapeJSON(query)),
					},
				})
			}
			return single(Event{Type: EventText})
		case "file_read", "file_edit", "file_change", "todo_list", "mcp_tool_call":
			return single(Event{Type: EventText})
		case "error":
			return single(Event{Type: EventError, Text: codexErrorItemText(ev.Item)})
		}
		return single(Event{Type: EventUnknown, Text: fmt.Sprintf("item.completed:%s", ev.Item.Type)})

	case "item.updated":
		// codex streams progress for in-flight items. Only todo_list updates
		// carry user-visible signal (checklist ticks); other types
		// (command_execution aggregated_output, file_change progress) just
		// repeat what item.started/item.completed already cover.
		if ev.Item == nil {
			return nil, false
		}
		if ev.Item.Type == "todo_list" {
			return single(Event{
				Type: EventTool,
				Tool: ToolUse{Name: "Plan", Input: codexPlanInput(ev.Item.Items)},
			})
		}
		return nil, false

	case "turn.completed":
		var e Event
		e.Type = EventResult
		if ev.Usage != nil {
			e.Usage.InputTokens = ev.Usage.InputTokens
			e.Usage.OutputTokens = ev.Usage.OutputTokens
			e.Usage.CacheRead = ev.Usage.CachedInputTokens
			// Codex reports cached_input_tokens as a subset of input_tokens.
			// For context occupancy we should use input_tokens directly to avoid
			// double-counting cached prompt tokens.
			e.Usage.ContextUsed = ev.Usage.InputTokens
		}
		return single(e)

	case "turn.started":
		return nil, false // expected, no info
	}

	return single(Event{Type: EventUnknown, Text: ev.Type})
}

func codexErrorItemText(item *codexItem) string {
	if item == nil {
		return "Codex item error"
	}
	msg := firstNonEmpty(
		item.Text,
		rawCodexMessage(item.Message),
		rawCodexMessage(item.Error),
	)
	if msg != "" {
		return "Codex item error: " + truncate(oneLinePreview(msg), toolPreviewLimit)
	}
	var parts []string
	if item.ID != "" {
		parts = append(parts, "id="+item.ID)
	}
	if item.Status != "" {
		parts = append(parts, "status="+item.Status)
	}
	if len(parts) == 0 {
		return "Codex item error"
	}
	return "Codex item error: " + strings.Join(parts, " ")
}

func rawCodexMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"message", "error", "detail", "details", "reason"} {
			if v, ok := obj[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return string(raw)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ReadSessionMeta reads model, effective context window, and the last turn's
// prompt size from the local Codex session JSONL file.
func ReadCodexSessionMeta(threadID string) (model string, contextWindow int, contextUsed int) {
	home, _ := os.UserHomeDir()
	if home == "" {
		return
	}
	sessDir := filepath.Join(home, ".codex", "sessions")
	// Find session file matching thread ID.
	pattern := filepath.Join(sessDir, "*", "*", "*", fmt.Sprintf("*%s.jsonl", threadID))
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return
	}
	defer f.Close()

	// Read with the same robust line reader as the runner's stdout loop, not a
	// bufio.Scanner: a codex rollout line can exceed any fixed scanner buffer (a
	// turn carrying large file contents or a big message). A scanner would hit
	// ErrTooLong and stop early, silently skipping the later token_count /
	// turn_context lines this function exists to pick up. readEventLine consumes
	// the whole file regardless, so the metadata is always found.
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := readEventLine(reader)
		if len(line) > 0 {
			var entry struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(line, &entry) == nil {
				switch entry.Type {
				case "event_msg":
					var ev struct {
						Type               string `json:"type"`
						ModelContextWindow int    `json:"model_context_window"`
						Info               *struct {
							LastTokenUsage *struct {
								InputTokens int `json:"input_tokens"`
							} `json:"last_token_usage"`
						} `json:"info,omitempty"`
					}
					if json.Unmarshal(entry.Payload, &ev) == nil {
						if ev.Type == "task_started" && ev.ModelContextWindow > 0 {
							contextWindow = ev.ModelContextWindow
						}
						if ev.Type == "token_count" && ev.Info != nil && ev.Info.LastTokenUsage != nil && ev.Info.LastTokenUsage.InputTokens > 0 {
							contextUsed = ev.Info.LastTokenUsage.InputTokens
						}
					}
				case "turn_context":
					var tc struct {
						Model string `json:"model"`
					}
					if json.Unmarshal(entry.Payload, &tc) == nil && tc.Model != "" {
						model = tc.Model
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	return
}

// codexPlanInput normalizes a codex todo_list event into the canonical
// PlanProgress JSON. Codex only tracks a per-item completed bool — we take
// the first incomplete item as "current", which matches how codex agents
// sequentially work through their plans.
func codexPlanInput(items []codexPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	done := 0
	current := ""
	for _, it := range items {
		if it.Completed {
			done++
		} else if current == "" {
			current = it.Text
		}
	}
	return MarshalPlanProgress(done, len(items), current)
}

// codexChangePaths returns a comma-separated list of paths from file_change events.
func codexChangePaths(changes []codexChange) string {
	if len(changes) == 1 {
		return changes[0].Path
	}
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	return strings.Join(paths, ", ")
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	// Remove surrounding quotes.
	return string(b[1 : len(b)-1])
}
