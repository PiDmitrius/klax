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
)

// CodexBackend implements Backend for OpenAI Codex CLI.
type CodexBackend struct {
	Sandbox  string // read-only | workspace-write | danger-full-access
	FullAuto bool
	APIKey   string
}

func (b *CodexBackend) Name() string { return "codex" }

func (b *CodexBackend) BuildCmd(opts RunOptions) (*exec.Cmd, error) {
	var args []string

	if opts.SessionID != "" {
		// Resume existing session.
		args = []string{"exec", "resume", opts.SessionID, "--json", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check"}
	}

	if b.FullAuto {
		args = append(args, "--full-auto")
	} else if b.Sandbox != "" {
		args = append(args, "--sandbox", b.Sandbox)
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
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

	// Set API key if configured.
	if b.APIKey != "" {
		cmd.Env = append(os.Environ(), "CODEX_API_KEY="+b.APIKey)
	}

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
	Type             string          `json:"type"` // agent_message | command_execution | web_search | file_read | file_edit | ...
	Text             string          `json:"text,omitempty"`
	Command          string          `json:"command,omitempty"`
	AggregatedOutput string          `json:"aggregated_output,omitempty"`
	ExitCode         *int            `json:"exit_code,omitempty"`
	Status           string          `json:"status,omitempty"`
	Query            string          `json:"query,omitempty"`   // web_search
	FilePath         string          `json:"file_path,omitempty"` // file_read, file_edit
	Action           json.RawMessage `json:"action,omitempty"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

func (b *CodexBackend) ParseEvent(line []byte) (Event, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, false
	}

	switch ev.Type {
	case "thread.started":
		return Event{
			Type:      "system",
			SessionID: ev.ThreadID,
		}, true

	case "item.started":
		if ev.Item == nil {
			return Event{}, false
		}
		switch ev.Item.Type {
		case "command_execution":
			return Event{
				Type: "tool",
				Tool: ToolUse{
					Name:  "Bash",
					Input: fmt.Sprintf(`{"command":"%s"}`, escapeJSON(ev.Item.Command)),
				},
			}, true
		case "web_search":
			return Event{
				Type: "tool",
				Tool: ToolUse{Name: "WebSearch", Input: ""},
			}, true
		case "file_read":
			return Event{
				Type: "tool",
				Tool: ToolUse{
					Name:  "Read",
					Input: fmt.Sprintf(`{"file_path":"%s"}`, escapeJSON(ev.Item.FilePath)),
				},
			}, true
		case "file_edit", "file_change":
			return Event{
				Type: "tool",
				Tool: ToolUse{
					Name:  "Edit",
					Input: fmt.Sprintf(`{"file_path":"%s"}`, escapeJSON(ev.Item.FilePath)),
				},
			}, true
		}
		return Event{Type: "unknown", Text: fmt.Sprintf("item.started:%s", ev.Item.Type)}, true

	case "item.completed":
		if ev.Item == nil {
			return Event{}, false
		}
		switch ev.Item.Type {
		case "agent_message":
			return Event{
				Type: "intermediate",
				Text: ev.Item.Text,
			}, true
		case "command_execution":
			return Event{Type: "text"}, true
		case "web_search":
			query := ev.Item.Query
			if query != "" {
				return Event{
					Type: "tool",
					Tool: ToolUse{
						Name:  "WebSearch",
						Input: fmt.Sprintf(`{"query":"%s"}`, escapeJSON(query)),
					},
				}, true
			}
			return Event{Type: "text"}, true
		case "file_read", "file_edit", "file_change":
			return Event{Type: "text"}, true
		}
		return Event{Type: "unknown", Text: fmt.Sprintf("item.completed:%s", ev.Item.Type)}, true

	case "turn.completed":
		var e Event
		e.Type = "result"
		if ev.Usage != nil {
			e.Usage.InputTokens = ev.Usage.InputTokens
			e.Usage.OutputTokens = ev.Usage.OutputTokens
			e.Usage.CacheRead = ev.Usage.CachedInputTokens
			// Codex reports cached_input_tokens as a subset of input_tokens.
			// For context occupancy we should use input_tokens directly to avoid
			// double-counting cached prompt tokens.
			e.Usage.ContextUsed = ev.Usage.InputTokens
		}
		return e, true

	case "turn.started":
		return Event{}, false // expected, no info
	}

	return Event{Type: "unknown", Text: ev.Type}, true
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
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
		// Stop after finding both.
	}
	return
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	// Remove surrounding quotes.
	return string(b[1 : len(b)-1])
}
