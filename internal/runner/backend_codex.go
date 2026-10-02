package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", opts.Effort))
	}

	// Prompt via stdin.
	args = append(args, "-")

	bin := FindBinary("codex")
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

type codexSessionMeta struct {
	Model         string
	ContextWindow int
}

// codexLeadRe reads the fixed leading keys Codex writes on every rollout record.
var codexLeadRe = regexp.MustCompile(`^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"(\w+)"(?:,"payload":\{"type":"(\w+)"(?:,"thread_id":"[^"]*")?(?:,"turn_id":"[^"]*")?(?:,"item":\{"type":"(\w+)")?)?`)

// CodexLead returns a rollout record's type, payload type and completed-item type from its
// leading keys, without decoding it. ok is false when the record does not start in Codex's
// fixed key order; the caller must then decode it.
func CodexLead(raw []byte) (record, payload, item string, ok bool) {
	m := codexLeadRe.FindSubmatch(raw[:min(len(raw), 512)])
	if m == nil {
		return "", "", "", false
	}
	return string(m[1]), string(m[2]), string(m[3]), true
}

// ParseCodexTerminalError returns the terminal failure carried by a complete
// rollout record. The rollout is the canonical source; callers never persist a
// second copy of the message.
func ParseCodexTerminalError(line []byte) string {
	message, _ := parseCodexTaskComplete(line)
	return message
}

func parseCodexTaskComplete(line []byte) (string, bool) {
	var entry struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "event_msg" {
		return "", false
	}
	var event struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"codex_error_info"`
		} `json:"error"`
	}
	if json.Unmarshal(entry.Payload, &event) != nil || event.Type != "task_complete" {
		return "", false
	}
	if event.Error == nil {
		return "", true
	}
	message := strings.TrimSpace(event.Error.Message)
	code := strings.TrimSpace(event.Error.Code)
	if message == "" {
		message = code
	} else if code != "" {
		message += " (" + code + ")"
	}
	return message, true
}

func codexSessionSize(threadID string) int64 {
	path := LocateCodexRollout(threadID)
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// readCodexRun reads the rollout bytes a run appended, from offset, in one pass: the failure
// carried by its last complete task_complete (empty when the run completed without one or
// wrote none), and the model and context window it ran with — Codex writes both in every
// turn. Records of other kinds are skipped by their leading keys, undecoded; an unfinished
// trailing record is ignored.
func readCodexRun(threadID string, offset int64) (terminal string, meta codexSessionMeta) {
	path := LocateCodexRollout(threadID)
	if path == "" || offset < 0 {
		return "", meta
	}
	f, err := os.Open(path)
	if err != nil {
		return "", meta
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.NewSectionReader(f, offset, math.MaxInt64-offset), 64*1024)
	for {
		line, err := readEventLine(r)
		if err != nil {
			return terminal, meta
		}
		if record, payload, _, ok := CodexLead(line); ok && !codexRunRecord(record, payload) {
			continue
		}
		if message, complete := parseCodexTaskComplete(line); complete {
			terminal = message
			continue
		}
		parseCodexSessionMetaLine(line, &meta)
	}
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
					Name:  "Exec",
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

// codexRunRecord reports whether a record classified by CodexLead can carry a run's terminal
// failure, model or window. An event whose payload type was not read must be decoded.
func codexRunRecord(record, payload string) bool {
	switch record {
	case "turn_context":
		return true
	case "event_msg":
		return payload == "" || payload == "task_started" || payload == "token_count" || payload == "task_complete"
	}
	return false
}

// codexRollouts caches located rollouts. A rollout's path is fixed once written, so a cached
// entry can only go stale by the file being removed, which the stat catches.
var codexRollouts sync.Map // threadID -> path

// LocateCodexRollout finds a Codex rollout by thread id, scanning ~/.codex/sessions only on a
// cache miss.
func LocateCodexRollout(threadID string) string {
	if threadID == "" {
		return ""
	}
	if v, ok := codexRollouts.Load(threadID); ok {
		p := v.(string)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		codexRollouts.Delete(threadID)
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*"+threadID+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	codexRollouts.Store(threadID, matches[0])
	return matches[0]
}

func parseCodexSessionMetaLine(line []byte, meta *codexSessionMeta) {
	var entry struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return
	}
	switch entry.Type {
	case "event_msg":
		var ev struct {
			Type               string `json:"type"`
			ModelContextWindow int    `json:"model_context_window"`
			Info               *struct {
				ModelContextWindow int `json:"model_context_window"`
			} `json:"info,omitempty"`
		}
		if json.Unmarshal(entry.Payload, &ev) != nil {
			return
		}
		if ev.Info != nil && ev.Info.ModelContextWindow > 0 {
			meta.ContextWindow = ev.Info.ModelContextWindow
		} else if ev.Type == "task_started" && ev.ModelContextWindow > 0 {
			meta.ContextWindow = ev.ModelContextWindow
		}
	case "turn_context":
		var tc struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(entry.Payload, &tc) == nil && tc.Model != "" {
			meta.Model = tc.Model
		}
	}
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
