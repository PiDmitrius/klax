package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	ID               string `json:"id"`
	Type             string `json:"type"` // agent_message | command_execution
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
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
		if ev.Item != nil && ev.Item.Type == "command_execution" {
			return Event{
				Type: "tool",
				Tool: ToolUse{
					Name:  "Bash",
					Input: fmt.Sprintf(`{"command":"%s"}`, escapeJSON(ev.Item.Command)),
				},
			}, true
		}
		return Event{}, false

	case "item.completed":
		if ev.Item == nil {
			return Event{}, false
		}
		switch ev.Item.Type {
		case "agent_message":
			// Intermediate thinking — show as progress, not final result.
			// The runner will accumulate these; only the last text before
			// turn.completed becomes the final answer.
			return Event{
				Type: "intermediate",
				Text: ev.Item.Text,
			}, true
		case "command_execution":
			// Tool finished — clear tool status.
			return Event{Type: "text"}, true
		}
		return Event{}, false

	case "turn.completed":
		var e Event
		e.Type = "result"
		if ev.Usage != nil {
			e.Usage.InputTokens = ev.Usage.InputTokens
			e.Usage.OutputTokens = ev.Usage.OutputTokens
			e.Usage.CacheRead = ev.Usage.CachedInputTokens
		}
		return e, true
	}

	return Event{}, false
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	// Remove surrounding quotes.
	return string(b[1 : len(b)-1])
}
