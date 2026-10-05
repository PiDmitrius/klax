// Package history turns a backend's session JSONL (Claude transcript or Codex
// rollout) into a common, UI-renderable list of turns. It feeds the web UI's read
// model (buildReadModel), so reopening the window restores the full session and any
// of them can be continued.
package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
	"github.com/PiDmitrius/klax/internal/promptcanon"
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
	name = runner.NormalizeToolName(name)
	return ToolCall{Name: name, Label: runner.ToolUse{Name: name, Input: input}.Preview(runner.UIToolPreviewLimit)}
}

func compactToolText(trigger string, preTokens, postTokens int) string {
	return runner.CompactToolUse(trigger, preTokens, postTokens, "").Preview(runner.UIToolPreviewLimit)
}

// Item is one entry in a rendered transcript.
type Item struct {
	Role      string     `json:"role"`           // "user" | "assistant" | "system" | "tool"
	Text      string     `json:"text,omitempty"` // message text (Markdown)
	Marker    string     `json:"-"`              // user turns: the klax-turn correlation token
	Tools     []ToolCall `json:"tools,omitempty"`
	Kind      string     `json:"kind,omitempty"` // "" | "error"
	Time      string     `json:"time,omitempty"` // RFC3339, empty when unknown
	CtxUsed   int        `json:"ctx_used,omitempty"`
	CtxWindow int        `json:"ctx_window,omitempty"`
	Seq       int64      `json:"-"` // durable turn_seq, set on pending turns surfaced from the queue
	// Pending drives the client's per-turn dots on reload: "" normal/done | "enq" still
	// queued | "run" started-but-not-yet-flushed-to-transcript. Lets a full reload show a
	// queued message exactly as it was instead of dropping it until it runs.
	Pending      string `json:"-"`
	Event        int64  `json:"-"` // zero-based complete physical JSONL record
	RecordDigest string `json:"-"`
	PromptDigest string `json:"-"` // canonical external-user payload, before display trimming
	Backend      string `json:"-"`
	Session      string `json:"-"`
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
	items, _, err := LoadGeneration(backend, sessionID, cwd)
	return items, err
}

// LoadGeneration is Load plus the transcript's index generation, which changes
// only when the file is re-indexed from zero, never on an append: a cache of
// rows derived from earlier items stays valid while it is unchanged.
func LoadGeneration(backend, sessionID, cwd string) ([]Item, uint64, error) {
	if sessionID == "" {
		return nil, 0, nil
	}
	snap, err := sessionTranscript(backend, sessionID, cwd)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	return snap.items, snap.gen, err
}

// Snapshot returns rendered items and the next complete physical event number.
func Snapshot(backend, sessionID, cwd string) ([]Item, int64, error) {
	if sessionID == "" {
		return nil, 0, nil
	}
	snap, err := sessionTranscript(backend, sessionID, cwd)
	return snap.items, int64(len(snap.ends)), err
}

func sessionTranscript(backend, sessionID, cwd string) (transcriptSnapshot, error) {
	var path string
	if backend == "codex" {
		path = runner.LocateCodexRollout(sessionID)
	} else {
		path = locateClaude(sessionID, cwd)
	}
	if path == "" {
		return transcriptSnapshot{}, fmt.Errorf("%s transcript %s: %w", backend, sessionID, os.ErrNotExist)
	}
	return loadTranscript(backend, sessionID, path)
}

// AuditSnapshot is the finish-side audit projection of one transcript
// snapshot: the whole-session context, the normalized turn slice, its physical
// coordinates, and the exact contiguous-byte digest.
type AuditSnapshot struct {
	Path          string
	FromEvent     int64
	ToEvent       int64
	SHA256        string
	Blocks        []Item
	ContextUsed   int
	ContextWindow int
}

func TurnAuditSnapshot(backend, sessionID, cwd string, fromEvent int64) (AuditSnapshot, error) {
	session, err := ReadAuditSession(backend, sessionID, cwd)
	if err != nil {
		return AuditSnapshot{}, err
	}
	return session.Turn(fromEvent)
}

// AuditSession is one immutable transcript snapshot used for binding, context,
// and turn-trace construction.
type AuditSession struct {
	Path          string
	ToEvent       int64
	Items         []Item
	ContextUsed   int
	ContextWindow int

	ends []int64
	file os.FileInfo
}

func ReadAuditSession(backend, sessionID, cwd string) (*AuditSession, error) {
	if sessionID == "" {
		return nil, errors.New("backend session id is empty")
	}
	snap, err := sessionTranscript(backend, sessionID, cwd)
	if err != nil {
		return nil, err
	}
	return newAuditSession(snap), nil
}

func newAuditSession(snap transcriptSnapshot) *AuditSession {
	used, window := latestContext(snap.items)
	return &AuditSession{
		Path: snap.path, ToEvent: int64(len(snap.ends)), Items: snap.items,
		ContextUsed: used, ContextWindow: window,
		ends: snap.ends, file: snap.file,
	}
}

// Turn is the snapshot's projection of one bound turn: its blocks as indexed and
// the SHA-256 of its exact byte span, from fromEvent to the snapshot's end. The
// transcript only grows (docs/CONTRACT.md), so those bytes are the ones indexed.
func (s *AuditSession) Turn(fromEvent int64) (AuditSnapshot, error) {
	if fromEvent < 0 || fromEvent >= s.ToEvent {
		return AuditSnapshot{}, fmt.Errorf("bound event %d outside transcript [0,%d)", fromEvent, s.ToEvent)
	}
	lead := slices.IndexFunc(s.Items, func(it Item) bool { return it.Role == "user" && it.Event == fromEvent })
	if lead < 0 {
		return AuditSnapshot{}, fmt.Errorf("event %d is not a normalized backend user record of this snapshot", fromEvent)
	}
	start := int64(0)
	if fromEvent > 0 {
		start = s.ends[fromEvent-1]
	}
	end := s.ends[len(s.ends)-1]
	f, err := os.Open(s.Path)
	if err != nil {
		return AuditSnapshot{}, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, s.file) {
		return AuditSnapshot{}, fmt.Errorf("transcript %s is no longer the indexed file", s.Path)
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.NewSectionReader(f, start, end-start)); err != nil || n != end-start {
		return AuditSnapshot{}, fmt.Errorf("read transcript events %d..%d: %d of %d bytes: %v", fromEvent, s.ToEvent, n, end-start, err)
	}
	return AuditSnapshot{
		Path: s.Path, FromEvent: fromEvent, ToEvent: s.ToEvent,
		SHA256: hex.EncodeToString(h.Sum(nil)), Blocks: s.Items[lead:],
		ContextUsed: s.ContextUsed, ContextWindow: s.ContextWindow,
	}, nil
}

// LatestContext returns a session's current context (used tokens, window) from the
// ONE canonical place: the transcript's last assistant message that reported usage —
// the exact value the read model draws on the timeline. The session strip, the
// settings modal, and the messenger all take their context from here (via the stored
// snapshot), so the number is identical on every surface. window is 0 for Claude (its
// transcript carries none); the caller falls back to the stream-reported window.
func LatestContext(backend, sessionID, cwd string) (used, window int) {
	items, _ := Load(backend, sessionID, cwd)
	return latestContext(items)
}

func latestContext(items []Item) (used, window int) {
	for _, it := range items {
		if it.Role == "assistant" && it.CtxUsed > 0 {
			used, window = it.CtxUsed, it.CtxWindow
		}
	}
	return used, window
}

// Stat returns the transcript file's mod time and size (zero values + ok=false when the session
// has no file yet). It is a cheap change-detector: a caller that caches something derived from the
// transcript (e.g. the UI unread-block count) can stat first and skip re-reading an unchanged file.
func Stat(backend, sessionID, cwd string) (modTime time.Time, size int64, ok bool) {
	if sessionID == "" {
		return time.Time{}, 0, false
	}
	var path string
	if backend == "codex" {
		path = runner.LocateCodexRollout(sessionID)
	} else {
		path = locateClaude(sessionID, cwd)
	}
	if path == "" {
		return time.Time{}, 0, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, 0, false
	}
	return fi.ModTime(), fi.Size(), true
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

type claudeParser struct {
	items            []Item
	backend, session string
}

func (p *claudeParser) append(it Item) {
	it.Backend, it.Session = p.backend, p.session
	p.items = append(p.items, it)
}

func (p *claudeParser) publish() []Item { return p.items[:len(p.items):len(p.items)] }

func (p *claudeParser) add(rec rawRecord) {
	raw := rec.Raw
	line, ok := transcript.Parse(raw) // skips blanks and sidechains
	if !ok {
		return
	}
	if line.IsMeta {
		return // SDK-injected internal row (e.g. image-view annotation), never a real message
	}
	ts := timeOrEmpty(line.Time)
	if line.Compact != nil {
		p.append(Item{Role: "tool", Text: compactToolText(line.Compact.Trigger, line.Compact.PreTokens, line.Compact.PostTokens), Time: ts})
		return
	}
	if line.IsAPIError {
		// The `error` field carries a machine code; a refusal puts the
		// sentence that names the cause, and the request id support asks
		// for, in the message text beside it. Show that when it exists.
		text, _, _ := claudeAssistant(raw)
		if text == "" {
			text = line.Error
		}
		p.append(Item{Role: "system", Kind: "error", Text: text, Time: ts})
		return
	}
	switch line.Type {
	case "user":
		if text, marker := claudeUserText(line.Raw); text != "" {
			if marker == "" {
				if compactText, ok := claudeCompactContinuationToolText(text); ok {
					p.append(Item{Role: "tool", Text: compactText, Time: ts})
					return
				}
				if claudeInternalCompactNoise(text) {
					return
				}
			}
			p.append(Item{Role: "user", Text: text, Marker: marker, Time: ts, Event: rec.Event, RecordDigest: rec.digest(), PromptDigest: claudeUserDigest(line.Raw)})
		}
	case "assistant":
		text, tools, ctxUsed := claudeAssistant(line.Raw)
		if text != "" || len(tools) > 0 {
			p.append(Item{Role: "assistant", Text: text, Tools: tools, Time: ts, CtxUsed: ctxUsed})
		}
	}
}

func claudeUserDigest(raw json.RawMessage) string {
	text, ok := claudeUserPayload(raw)
	if !ok {
		return ""
	}
	return promptcanon.Digest(text)
}

func claudeUserPayload(raw json.RawMessage) (string, bool) {
	var w struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(w.Message.Content, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(w.Message.Content, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}

// claudeUserText pulls the real user text out of a user line. content is either
// a plain string (a typed message) or an array of blocks; a user line whose
// array holds only tool_result blocks (tool output fed back to the model) has no
// user text and is skipped.
func claudeUserText(raw json.RawMessage) (clean, marker string) {
	s, ok := claudeUserPayload(raw)
	if !ok {
		return "", ""
	}
	return StripTurnMarker(s)
}

// Claude writes its own compaction/resume summary as a role=user transcript
// row. It is internal mechanics, not human input, but it is still useful
// timeline data, so render it as a tool-style agent event.
func claudeCompactContinuationToolText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "This session is being continued from a previous conversation that ran out of context.") &&
		(strings.Contains(text, "\n\nSummary:") || strings.Contains(text, "\nSummary:")) {
		return runner.CompactToolUse("", 0, 0, text).Preview(runner.UIToolPreviewLimit), true
	}
	return "", false
}

// Manual /compact also writes command bookkeeping rows as role=user. Those are
// transport noise around the compact boundary and summary, not user messages.
func claudeInternalCompactNoise(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<local-command-caveat>") && strings.Contains(text, "</local-command-caveat>") {
		return true
	}
	if strings.HasPrefix(text, "<command-name>/compact</command-name>") && strings.Contains(text, "<command-message>compact</command-message>") {
		return true
	}
	if strings.HasPrefix(text, "<local-command-stdout>") && strings.Contains(text, "Compacted (") {
		return true
	}
	return false
}

func claudeAssistant(raw json.RawMessage) (string, []ToolCall, int) {
	var w struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage *struct {
				InputTokens         int `json:"input_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return "", nil, 0
	}
	var sb strings.Builder
	var tools []ToolCall
	for _, b := range w.Message.Content {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			tools = append(tools, toolCall(runner.NormalizeClaudeToolUse(b.Name, b.Input)))
		}
	}
	ctxUsed := 0
	if w.Message.Usage != nil {
		ctxUsed = w.Message.Usage.InputTokens + w.Message.Usage.CacheReadTokens + w.Message.Usage.CacheCreationTokens
	}
	return strings.TrimSpace(sb.String()), tools, ctxUsed
}

// ---- Codex rollout ----

type codexParser struct {
	items            []Item
	shared           int // items[:shared] are published and amended only on a private copy
	backend, session string
	lastAssistant    int
	lastWasCompacted bool
	lastCompacted    int
}

func (c *codexParser) publish() []Item {
	c.shared = len(c.items)
	return c.items[:len(c.items):len(c.items)]
}

func (c *codexParser) at(i int) *Item {
	if i < c.shared {
		c.items, c.shared = slices.Clone(c.items), 0
	}
	return &c.items[i]
}

func (c *codexParser) append(it Item) {
	it.Backend, it.Session = c.backend, c.session
	c.items = append(c.items, it)
}

func (c *codexParser) appendAssistant(it Item) {
	c.append(it)
	c.lastAssistant = len(c.items) - 1
	c.lastWasCompacted = false
	c.lastCompacted = -1
}

func (c *codexParser) appendTool(tc ToolCall, ts string) {
	c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{tc}, Time: ts})
}

func (c *codexParser) appendUser(message, ts string, rec rawRecord) {
	if t, marker := StripTurnMarker(message); t != "" {
		c.append(Item{Role: "user", Text: t, Marker: marker, Time: ts, Event: rec.Event, RecordDigest: rec.digest(), PromptDigest: promptcanon.Digest(message)})
		c.lastAssistant = -1
	}
	c.lastWasCompacted = false
	c.lastCompacted = -1
}

func (c *codexParser) appendAgent(message, ts string) {
	if t := strings.TrimSpace(message); t != "" {
		c.appendAssistant(Item{Role: "assistant", Text: t, Time: ts})
	}
}

// codexSilent reports a record that never renders: tool output, reasoning, raw model messages
// and completed items other than messages. They are most of a rollout's bytes, so they are
// recognized by their leading keys and skip JSON decoding.
func codexSilent(raw []byte) bool {
	record, payload, item, ok := runner.CodexLead(raw)
	if !ok {
		return false
	}
	switch record + "/" + payload {
	case "response_item/function_call_output", "response_item/custom_tool_call_output", "response_item/reasoning", "response_item/message":
		return true
	case "event_msg/item_completed":
		return item != "" && item != "UserMessage" && item != "AgentMessage"
	}
	return false
}

func (c *codexParser) add(rec rawRecord) {
	raw := rec.Raw
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || codexSilent(raw) {
		return
	}
	var entry struct {
		Type      string            `json:"type"`
		Timestamp string            `json:"timestamp"`
		Payload   json.RawMessage   `json:"payload"`
		Item      *codexHistoryItem `json:"item"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return
	}
	if entry.Type != "event_msg" && entry.Type != "response_item" && entry.Type != "compacted" && !strings.HasPrefix(entry.Type, "item.") {
		return
	}
	var p struct {
		Type      string          `json:"type"`
		Message   string          `json:"message"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Action    json.RawMessage `json:"action"`
		Item      *codexEventItem `json:"item"`
		Info      *struct {
			LastTokenUsage *struct {
				InputTokens int `json:"input_tokens"`
			} `json:"last_token_usage"`
			ModelContextWindow int `json:"model_context_window"`
		} `json:"info"`
	}
	_ = json.Unmarshal(entry.Payload, &p)
	ts := normalizeTime(entry.Timestamp)
	switch {
	case entry.Type == "event_msg" && p.Type == "task_complete":
		if message := runner.ParseCodexTerminalError(raw); message != "" {
			c.append(Item{Role: "system", Kind: "error", Text: message, Time: ts})
			c.lastAssistant = -1
		}
	case entry.Type == "compacted":
		c.append(Item{Role: "tool", Text: compactToolText("", 0, 0), Time: ts})
		c.lastAssistant = -1
		c.lastWasCompacted = true
		c.lastCompacted = len(c.items) - 1
	case entry.Type == "event_msg" && p.Type == "user_message":
		c.appendUser(p.Message, ts, rec)
	case entry.Type == "event_msg" && p.Type == "agent_message":
		c.appendAgent(p.Message, ts)
	case entry.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil:
		// Codex carries conversation text as completed items; only the two message
		// kinds are read here, because every other kind reaches the timeline through
		// the response_item tool records and would otherwise be drawn twice.
		switch p.Item.Type {
		case "UserMessage":
			c.appendUser(p.Item.text(), ts, rec)
		case "AgentMessage":
			c.appendAgent(p.Item.text(), ts)
		}
	case entry.Type == "event_msg" && p.Type == "context_compacted":
		if !c.lastWasCompacted {
			c.append(Item{Role: "tool", Text: compactToolText("", 0, 0), Time: ts})
			c.lastCompacted = len(c.items) - 1
		} else if c.lastCompacted >= 0 && c.items[c.lastCompacted].Time == "" {
			c.at(c.lastCompacted).Time = ts
		}
		c.lastAssistant = -1
		c.lastWasCompacted = true
	case entry.Type == "event_msg" && p.Type == "token_count" && c.lastAssistant >= 0:
		if p.Info != nil && p.Info.LastTokenUsage != nil {
			it := c.at(c.lastAssistant)
			it.CtxUsed, it.CtxWindow = p.Info.LastTokenUsage.InputTokens, p.Info.ModelContextWindow
		}
	case entry.Type == "item.started":
		if tool, ok := codexHistoryItemTool(entry.Item); ok {
			c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{tool}, Time: ts})
		}
	case entry.Type == "item.completed" && entry.Item != nil && entry.Item.Type == "web_search" && entry.Item.Query != "":
		c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{toolCall("WebSearch", jsonObject("query", entry.Item.Query))}, Time: ts})
	case entry.Type == "response_item" && p.Type == "custom_tool_call" && p.Name == "exec":
		// New Codex orchestration wrapper: its JavaScript `input` invokes one or more
		// tools.<name>(...) actions. Decode them into real tool rows (Exec, Write, …).
		// If nothing decodes, fall back to showing the RAW orchestration source as an Exec
		// row (truncated by the preview) so the user still sees what Codex ran, rather than
		// an opaque 🔧 exec; a row is never silently dropped.
		src := rawJSONArgument(p.Input)
		if tools := decodeCodexExecTools(src); len(tools) > 0 {
			for _, tc := range tools {
				c.appendTool(tc, ts)
			}
		} else if src != "" {
			c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{toolCall("Exec", jsonObject("command", src))}, Time: ts})
		} else {
			c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{{Name: "exec", Label: "🔧 exec"}}, Time: ts})
		}
	case entry.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call"):
		if p.Name != "" {
			args := rawJSONArgument(p.Arguments)
			if args == "" {
				args = rawJSONArgument(p.Input) // custom_tool_call carries "input" instead of "arguments"
			}
			c.appendTool(codexResponseToolCall(p.Namespace, p.Name, args), ts)
		}
	case entry.Type == "response_item" && p.Type == "web_search_call":
		if tool, ok := codexWebSearchTool(p.Action); ok {
			c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{tool}, Time: ts})
		}
	case entry.Type == "response_item" && p.Type == "tool_search_call":
		if query := jsonStringField(rawJSONArgument(p.Arguments), "query"); query != "" {
			c.appendAssistant(Item{Role: "assistant", Tools: []ToolCall{{Name: "ToolSearch", Label: "🔎 Tool search: " + query}}, Time: ts})
		}
	}
}

// codexEventItem is a completed conversation item; its content parts all carry the
// text under the same field regardless of part kind.
type codexEventItem struct {
	Type    string `json:"type"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (it *codexEventItem) text() string {
	var b strings.Builder
	for _, part := range it.Content {
		if part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return b.String()
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
		return toolCall("Exec", jsonObject("command", item.Command)), true
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

// codexWriteStdinTool maps a Codex write_stdin call (structured or decoded from an exec wrapper) to a
// row: an empty `chars` is the same user-visible progress event as the dedicated Wait tool;
// non-empty `chars` is actual input sent to the command's stdin. Each poll stays visible because
// each transcript record is a real progress step, not duplicate presentation state.
func codexWriteStdinTool(chars string) ToolCall {
	if chars == "" {
		return toolCall("Wait", "")
	}
	return toolCall("Exec", jsonObject("command", "ввод: "+chars))
}

func codexResponseToolCall(namespace, name, input string) ToolCall {
	switch name {
	case "exec_command":
		if cmd := jsonStringField(input, "cmd", "command"); cmd != "" {
			return toolCall("Exec", jsonObject("command", cmd))
		}
	case "write_stdin":
		var inp struct {
			Chars string `json:"chars"`
		}
		if json.Unmarshal([]byte(input), &inp) == nil {
			return codexWriteStdinTool(inp.Chars)
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
	case "web__run", "web_search", "web_fetch":
		return ToolCall{Name: "Web", Label: "🌐 Web"}
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
