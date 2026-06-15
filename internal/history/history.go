// Package history turns a backend's session JSONL (Claude transcript or Codex
// rollout) into a common, UI-renderable list of turns. It is the read model
// behind the web UI's /api/transcript: the live SSE stream covers "from now on",
// this covers everything before — so reopening the window restores the full
// session and any of them can be continued.
package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
	"github.com/PiDmitrius/klax/internal/runner"
)

// ToolCall is a tool invocation surfaced inside an assistant turn. Label is the
// same rich short label the live stream and Telegram show (via ToolUse.String).
type ToolCall struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

func toolCall(name, input string) ToolCall {
	return ToolCall{Name: name, Label: runner.ToolUse{Name: name, Input: input}.String()}
}

// Item is one entry in a rendered transcript.
type Item struct {
	Role  string     `json:"role"`           // "user" | "assistant" | "system"
	Text  string     `json:"text,omitempty"` // message text (Markdown)
	Tools []ToolCall `json:"tools,omitempty"`
	Kind  string     `json:"kind,omitempty"` // "" | "compact" | "error"
	Time  string     `json:"time,omitempty"` // RFC3339, empty when unknown
}

// Load locates and reads the transcript for a session. A missing file or empty
// session id yields (nil, nil) so callers degrade to "live only" rather than
// erroring.
func Load(backend, sessionID, cwd string) ([]Item, error) {
	if sessionID == "" {
		return nil, nil
	}
	if backend == "codex" {
		path := locateCodex(sessionID)
		if path == "" {
			return nil, nil
		}
		return readCodex(path)
	}
	path := locateClaude(sessionID, cwd)
	if path == "" {
		return nil, nil
	}
	return readClaude(path)
}

// ---- Claude transcript ----

func locateClaude(sessionID, cwd string) string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	// Fast path: Claude Code stores each session under a project dir whose name
	// is the cwd with path punctuation flattened to '-'.
	p := filepath.Join(home, ".claude", "projects", encodeProjectDir(cwd), sessionID+".jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// Robust fallback: the session id is globally unique, so find it in any
	// project dir even if the cwd encoding does not match exactly.
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func encodeProjectDir(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd)
}

func readClaude(path string) ([]Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line, ok := transcript.Parse(raw) // skips blanks and sidechains
		if !ok {
			continue
		}
		ts := timeOrEmpty(line.Time)
		if line.Compact != nil {
			items = append(items, Item{Role: "system", Kind: "compact", Time: ts})
			continue
		}
		if line.IsAPIError {
			items = append(items, Item{Role: "system", Kind: "error", Text: line.Error, Time: ts})
			continue
		}
		switch line.Type {
		case "user":
			if text := claudeUserText(line.Raw); text != "" {
				items = append(items, Item{Role: "user", Text: text, Time: ts})
			}
		case "assistant":
			text, tools := claudeAssistant(line.Raw)
			if text != "" || len(tools) > 0 {
				items = append(items, Item{Role: "assistant", Text: text, Tools: tools, Time: ts})
			}
		}
	}
	return items, nil
}

// claudeUserText pulls the real user text out of a user line. content is either
// a plain string (a typed message) or an array of blocks; a user line whose
// array holds only tool_result blocks (tool output fed back to the model) has no
// user text and is skipped.
func claudeUserText(raw json.RawMessage) string {
	var w struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(w.Message.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(w.Message.Content, &blocks)
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}

func claudeAssistant(raw json.RawMessage) (string, []ToolCall) {
	var w struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return "", nil
	}
	var sb strings.Builder
	var tools []ToolCall
	for _, b := range w.Message.Content {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			tools = append(tools, toolCall(b.Name, string(b.Input)))
		}
	}
	return strings.TrimSpace(sb.String()), tools
}

// ---- Codex rollout ----

func locateCodex(threadID string) string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*"+threadID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func readCodex(path string) ([]Item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, raw := range bytes.Split(data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var entry struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Type != "event_msg" && entry.Type != "response_item" {
			continue
		}
		var p struct {
			Type      string `json:"type"`
			Message   string `json:"message"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Input     string `json:"input"`
		}
		_ = json.Unmarshal(entry.Payload, &p)
		ts := normalizeTime(entry.Timestamp)
		switch {
		case entry.Type == "event_msg" && p.Type == "user_message":
			if t := strings.TrimSpace(p.Message); t != "" {
				items = append(items, Item{Role: "user", Text: t, Time: ts})
			}
		case entry.Type == "event_msg" && p.Type == "agent_message":
			if t := strings.TrimSpace(p.Message); t != "" {
				items = append(items, Item{Role: "assistant", Text: t, Time: ts})
			}
		case entry.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call"):
			if p.Name != "" {
				args := p.Arguments
				if args == "" {
					args = p.Input // custom_tool_call carries "input" instead of "arguments"
				}
				items = append(items, Item{Role: "assistant", Tools: []ToolCall{toolCall(p.Name, args)}, Time: ts})
			}
		}
	}
	return items, nil
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// normalizeTime reformats a Codex rollout ISO timestamp to RFC3339 (matching the
// Claude branch), passing it through unchanged if it does not parse.
func normalizeTime(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Format(time.RFC3339)
	}
	return s
}
