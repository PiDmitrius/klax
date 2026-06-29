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
	"regexp"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
	"github.com/PiDmitrius/klax/internal/runner"
)

// ToolCall is a tool invocation surfaced inside an assistant turn. Label is the
// same rich label the live UI stream shows, rendered at the wider web-UI width
// (ToolUse.Preview(UIToolPreviewLimit)) rather than the narrow Telegram one.
type ToolCall struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

func toolCall(name, input string) ToolCall {
	return ToolCall{Name: name, Label: runner.ToolUse{Name: name, Input: input}.Preview(runner.UIToolPreviewLimit)}
}

// Item is one entry in a rendered transcript.
type Item struct {
	Role   string     `json:"role"`             // "user" | "assistant" | "system"
	Text   string     `json:"text,omitempty"`   // message text (Markdown)
	Marker string     `json:"marker,omitempty"` // user turns: the klax-turn correlation token
	Tools  []ToolCall `json:"tools,omitempty"`
	Kind   string     `json:"kind,omitempty"` // "" | "compact" | "error"
	Time   string     `json:"time,omitempty"` // RFC3339, empty when unknown
	Seq    int64      `json:"seq,omitempty"`  // durable turn_seq, set on pending turns surfaced from the queue
	// Pending drives the client's per-turn dots on reload: "" normal/done | "enq" still
	// queued | "run" started-but-not-yet-flushed-to-transcript. Lets a full reload show a
	// queued message exactly as it was instead of dropping it until it runs.
	Pending string `json:"pending,omitempty"`
}

// turnMarkerRe matches ONLY klax's injected marker shape: the exact 16-hex token
// newMarker produces, at the end of the message (where buildTurnPrompt appends it),
// so a user message that merely contains a klax-turn-looking comment is left intact.
var turnMarkerRe = regexp.MustCompile(`\s*<!--\s*klax-turn:([0-9a-fA-F]{16})\s*-->\s*$`)

// StripTurnMarker removes the per-turn correlation marker that buildTurnPrompt
// injects into the prompt (so it never shows in rendered user text) and returns the
// cleaned, trimmed text plus the marker token (empty if absent). The token is the
// key that correlates a transcript user turn to its durable-queue turn.
func StripTurnMarker(text string) (clean, marker string) {
	if m := turnMarkerRe.FindStringSubmatch(text); m != nil {
		marker = m[1]
	}
	return strings.TrimSpace(turnMarkerRe.ReplaceAllString(text, "")), marker
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
			if text, marker := claudeUserText(line.Raw); text != "" {
				items = append(items, Item{Role: "user", Text: text, Marker: marker, Time: ts})
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
func claudeUserText(raw json.RawMessage) (clean, marker string) {
	var w struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return "", ""
	}
	var s string
	if json.Unmarshal(w.Message.Content, &s) == nil {
		return StripTurnMarker(s)
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
	return StripTurnMarker(sb.String())
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
			Type      string            `json:"type"`
			Timestamp string            `json:"timestamp"`
			Payload   json.RawMessage   `json:"payload"`
			Item      *codexHistoryItem `json:"item"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Type != "event_msg" && entry.Type != "response_item" && !strings.HasPrefix(entry.Type, "item.") {
			continue
		}
		var p struct {
			Type      string          `json:"type"`
			Message   string          `json:"message"`
			Name      string          `json:"name"`
			Namespace string          `json:"namespace"`
			Arguments json.RawMessage `json:"arguments"`
			Input     json.RawMessage `json:"input"`
			Action    json.RawMessage `json:"action"`
		}
		_ = json.Unmarshal(entry.Payload, &p)
		ts := normalizeTime(entry.Timestamp)
		switch {
		case entry.Type == "event_msg" && p.Type == "user_message":
			if t, marker := StripTurnMarker(p.Message); t != "" {
				items = append(items, Item{Role: "user", Text: t, Marker: marker, Time: ts})
			}
		case entry.Type == "event_msg" && p.Type == "agent_message":
			if t := strings.TrimSpace(p.Message); t != "" {
				items = append(items, Item{Role: "assistant", Text: t, Time: ts})
			}
		case entry.Type == "item.started":
			if tool, ok := codexHistoryItemTool(entry.Item); ok {
				items = append(items, Item{Role: "assistant", Tools: []ToolCall{tool}, Time: ts})
			}
		case entry.Type == "item.completed" && entry.Item != nil && entry.Item.Type == "web_search" && entry.Item.Query != "":
			items = append(items, Item{Role: "assistant", Tools: []ToolCall{toolCall("WebSearch", jsonObject("query", entry.Item.Query))}, Time: ts})
		case entry.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call"):
			if p.Name != "" {
				args := rawJSONArgument(p.Arguments)
				if args == "" {
					args = rawJSONArgument(p.Input) // custom_tool_call carries "input" instead of "arguments"
				}
				items = append(items, Item{Role: "assistant", Tools: []ToolCall{codexResponseToolCall(p.Namespace, p.Name, args)}, Time: ts})
			}
		case entry.Type == "response_item" && p.Type == "web_search_call":
			if tool, ok := codexWebSearchTool(p.Action); ok {
				items = append(items, Item{Role: "assistant", Tools: []ToolCall{tool}, Time: ts})
			}
		case entry.Type == "response_item" && p.Type == "tool_search_call":
			if query := jsonStringField(rawJSONArgument(p.Arguments), "query"); query != "" {
				items = append(items, Item{Role: "assistant", Tools: []ToolCall{{Name: "ToolSearch", Label: "🔎 Tool search: " + query}}, Time: ts})
			}
		}
	}
	return items, nil
}

type codexHistoryItem struct {
	Type     string                 `json:"type"`
	Command  string                 `json:"command,omitempty"`
	Query    string                 `json:"query,omitempty"`
	FilePath string                 `json:"file_path,omitempty"`
	Changes  []codexHistoryChange   `json:"changes,omitempty"`
	Server   string                 `json:"server,omitempty"`
	Tool     string                 `json:"tool,omitempty"`
	Items    []codexHistoryPlanItem `json:"items,omitempty"`
}

type codexHistoryChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type codexHistoryPlanItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

func codexHistoryItemTool(item *codexHistoryItem) (ToolCall, bool) {
	if item == nil {
		return ToolCall{}, false
	}
	switch item.Type {
	case "command_execution":
		return toolCall("Bash", jsonObject("command", item.Command)), true
	case "web_search":
		if item.Query != "" {
			return toolCall("WebSearch", jsonObject("query", item.Query)), true
		}
		return toolCall("WebSearch", ""), true
	case "file_read":
		return toolCall("Read", jsonObject("file_path", item.FilePath)), true
	case "file_edit":
		return toolCall("Edit", jsonObject("file_path", item.FilePath)), true
	case "file_change":
		name := "Edit"
		if len(item.Changes) == 1 && item.Changes[0].Kind == "add" {
			name = "Write"
		}
		return toolCall(name, jsonObject("file_path", codexHistoryChangePaths(item.Changes))), true
	case "todo_list":
		return toolCall("Plan", codexHistoryPlanInput(item.Items)), true
	case "mcp_tool_call":
		return toolCall("MCP", mcpInput(item.Server, item.Tool)), true
	}
	return ToolCall{}, false
}

func codexResponseToolCall(namespace, name, input string) ToolCall {
	switch name {
	case "exec_command":
		if cmd := jsonStringField(input, "cmd", "command"); cmd != "" {
			return toolCall("Bash", jsonObject("command", cmd))
		}
	case "write_stdin":
		var inp struct {
			SessionID int    `json:"session_id"`
			Chars     string `json:"chars"`
		}
		if json.Unmarshal([]byte(input), &inp) == nil && inp.SessionID != 0 {
			action := "wait for command session"
			if inp.Chars != "" {
				action = "write to command session"
			}
			return toolCall("Bash", jsonObject("command", action+" "+itoa(inp.SessionID)))
		}
	case "view_image":
		if path := jsonStringField(input, "path"); path != "" {
			return ToolCall{Name: "ViewImage", Label: "🖼️ Image: " + path}
		}
	case "apply_patch":
		if paths := patchPaths(input); len(paths) > 0 {
			name := "Edit"
			if len(paths) == 1 && strings.HasPrefix(input, "*** Begin Patch\n*** Add File: ") {
				name = "Write"
			}
			return toolCall(name, jsonObject("file_path", strings.Join(paths, ", ")))
		}
	case "update_plan":
		if plan := codexPlanFromFunctionArgs(input); plan != "" {
			return toolCall("Plan", plan)
		}
	}
	if namespace != "" {
		if server, ok := codexMCPNamespace(namespace); ok {
			return toolCall("MCP", mcpInput(server, name))
		}
	}
	if server, tool, ok := codexMCPFunctionName(name); ok {
		return toolCall("MCP", mcpInput(server, tool))
	}
	return toolCall(name, input)
}

func rawJSONArgument(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func jsonStringField(raw string, keys ...string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return ""
	}
	for _, key := range keys {
		var s string
		if json.Unmarshal(obj[key], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

func jsonObject(key, value string) string {
	b, _ := json.Marshal(map[string]string{key: value})
	return string(b)
}

func itoa(n int) string {
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if i == len(buf) {
		i--
		buf[i] = '0'
	}
	return string(buf[i:])
}

func mcpInput(server, tool string) string {
	b, _ := json.Marshal(map[string]string{"server": server, "tool": tool})
	return string(b)
}

func codexMCPNamespace(namespace string) (string, bool) {
	if !strings.HasPrefix(namespace, "mcp__") {
		return "", false
	}
	server := strings.TrimPrefix(namespace, "mcp__")
	server = strings.ReplaceAll(server, "__", ".")
	return server, server != ""
}

func codexMCPFunctionName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(name, "mcp__"), "__")
	if len(parts) < 2 {
		return "", "", false
	}
	return strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1], true
}

func patchPaths(patch string) []string {
	var paths []string
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if path, ok := strings.CutPrefix(line, prefix); ok {
				paths = append(paths, strings.TrimSpace(path))
			}
		}
	}
	return paths
}

func codexPlanFromFunctionArgs(input string) string {
	var inp struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal([]byte(input), &inp) != nil || len(inp.Plan) == 0 {
		return ""
	}
	done := 0
	current := ""
	for _, item := range inp.Plan {
		if item.Status == "completed" {
			done++
		} else if current == "" && item.Step != "" {
			current = item.Step
		}
	}
	return runner.MarshalPlanProgress(done, len(inp.Plan), current)
}

func codexHistoryPlanInput(items []codexHistoryPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	done := 0
	current := ""
	for _, item := range items {
		if item.Completed {
			done++
		} else if current == "" {
			current = item.Text
		}
	}
	return runner.MarshalPlanProgress(done, len(items), current)
}

func codexHistoryChangePaths(changes []codexHistoryChange) string {
	if len(changes) == 1 {
		return changes[0].Path
	}
	var paths []string
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return strings.Join(paths, ", ")
}

func codexWebSearchTool(raw json.RawMessage) (ToolCall, bool) {
	var action struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
		Pattern string   `json:"pattern"`
	}
	if json.Unmarshal(raw, &action) != nil {
		return ToolCall{}, false
	}
	switch action.Type {
	case "search":
		if action.Query == "" && len(action.Queries) > 0 {
			action.Query = action.Queries[0]
		}
		if action.Query != "" {
			return toolCall("WebSearch", jsonObject("query", action.Query)), true
		}
		return toolCall("WebSearch", ""), true
	case "open_page":
		if action.URL != "" {
			return toolCall("WebFetch", jsonObject("url", action.URL)), true
		}
		return ToolCall{Name: "WebFetch", Label: "🌐 Fetch"}, true
	case "find_in_page":
		label := strings.TrimSpace(action.Pattern)
		if action.URL != "" {
			if label != "" {
				label += " in " + action.URL
			} else {
				label = action.URL
			}
		}
		if label == "" {
			return ToolCall{Name: "WebFind", Label: "🌐 Find in page"}, true
		}
		return ToolCall{Name: "WebFind", Label: "🌐 Find: " + label}, true
	}
	return ToolCall{}, false
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
