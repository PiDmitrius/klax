// Package runner executes AI CLI tools and streams results.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/PiDmitrius/klax/internal/fmtutil"
	"github.com/PiDmitrius/klax/internal/pathutil"
)

// killGracePeriod is how long we wait for a cancelled process group to exit
// on its own after SIGTERM before following up with SIGKILL. Tune if backends
// start doing meaningful cleanup on shutdown.
const killGracePeriod = 3 * time.Second

// Narration look-ahead tuning. These gate when the runner is allowed to leak
// a partial assistant reply out as ProgressKindNarration *before* the usual
// tool/end boundary. The invariant the user relies on is "trailing `...` in
// the visible message ⇔ work is still in flight". We preserve it by only
// flushing a body once we can prove a tail is queued behind it (look-ahead),
// and by only firing the idle fallback while the backend process is still
// alive. Vars (not consts) so tests can shrink the idle window.
var (
	// narrationFlushMinChars is the body size that has to accumulate before
	// a look-ahead flush is even considered.
	narrationFlushMinChars = 200
	// narrationFlushLookaheadChars is the tail we keep in the buffer after a
	// look-ahead flush as proof that more was coming. Without this tail we
	// would be guessing — with it, the flush is justified by the bytes we
	// actually held back.
	narrationFlushLookaheadChars = 100
	// narrationIdleTimeout is the ceiling on quiet time between two emits
	// from the same pending segment. If it expires with text still buffered,
	// we flush so the user keeps seeing the model is alive.
	narrationIdleTimeout = 4 * time.Second
)

// ToolUse describes what the AI is doing right now.
//
// Input is a JSON string whose shape is the *canonical* form expected by
// ToolUse.String() — backends must normalize their native event payloads into
// this form before constructing a ToolUse. Per-tool schemas:
//
//	Bash        {"command": string}
//	Read        {"file_path": string}
//	Edit        {"file_path": string}
//	Write       {"file_path": string}
//	WebFetch    {"url": string}
//	WebSearch   {"query": string}
//	Glob        {"pattern": string}
//	Grep        {"pattern": string}
//	LS          {"path": string}
//	Task        {"description": string}                — sub-agent spawn
//	Plan        PlanProgress (see below)               — checklist snapshot
//	TaskCreate  {"subject": string}                    — harness task list, single create
//	TaskUpdate  {"taskId": string, "status": string, "subject": string, "activeForm": string}
//	TaskList    {}
//	TaskGet     {"taskId": string}
//	MCP         {"server": string, "tool": string}
//
// Unknown tool names render as "🔧 <name>" with no input inspection.
type ToolUse struct {
	Name  string
	Input string
}

// PlanProgress is the canonical Plan payload — both Claude's TodoWrite
// (`{todos:[{content,status,activeForm}]}`) and codex's todo_list items[]
// events are normalized into this form by their respective backends. Current
// is the in-progress (or first pending) item's user-visible label.
type PlanProgress struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
}

// MarshalPlanProgress packs done/total/current into the canonical JSON form
// that ToolUse{Name:"Plan"}.String() expects.
func MarshalPlanProgress(done, total int, current string) string {
	b, _ := json.Marshal(PlanProgress{Done: done, Total: total, Current: current})
	return string(b)
}

const toolPreviewLimit = 120

// UIToolPreviewLimit is the wider tool-label width used by frontends with more
// room than a Telegram chat (the web UI). They pass it to Preview instead of
// the default toolPreviewLimit, so commands, queries and descriptions render
// in near-full rather than being clipped to a chat-sized line.
const UIToolPreviewLimit = 256

// String renders the tool label at the default (Telegram-width) preview limit.
func (t ToolUse) String() string { return t.Preview(toolPreviewLimit) }

// Preview renders the tool label, truncating the one variable-length field
// (command, URL, query, description, plan/task subject) to limit runes. Fixed,
// inherently short fields like file paths and patterns are never truncated.
func (t ToolUse) Preview(limit int) string {
	switch t.Name {
	case "Bash":
		var inp struct{ Command string }
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("⚙️ Bash: `%s`", truncate(oneLinePreview(inp.Command), limit))
	case "Read":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📖 Read: %s", inp.FilePath)
	case "Edit", "MultiEdit":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("✏️ Edit: %s", inp.FilePath)
	case "Write":
		var inp struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📝 Write: %s", inp.FilePath)
	case "WebFetch":
		var inp struct {
			URL string `json:"url"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🌐 Fetch: %s", truncate(inp.URL, limit))
	case "WebSearch":
		var inp struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Query != "" {
			return fmt.Sprintf("🔎 Search: %s", truncate(inp.Query, limit))
		}
		return "🔎 Search..."
	case "Glob", "GlobTool":
		var inp struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🔍 Glob: %s", inp.Pattern)
	case "Grep", "GrepTool":
		var inp struct {
			Pattern string `json:"pattern"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("🔍 Grep: %s", inp.Pattern)
	case "LS":
		var inp struct {
			Path string `json:"path"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("📂 LS: %s", inp.Path)
	case "Task":
		var inp struct {
			Description string `json:"description"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Description != "" {
			return fmt.Sprintf("🤖 Task: %s", truncate(inp.Description, limit))
		}
		return "🤖 Task"
	case "Plan":
		var p PlanProgress
		if t.Input != "" {
			json.Unmarshal([]byte(t.Input), &p)
		}
		if p.Total == 0 {
			return "📌 Plan"
		}
		if p.Done >= p.Total {
			return fmt.Sprintf("📌 ✓ %d/%d", p.Done, p.Total)
		}
		if p.Current != "" {
			return fmt.Sprintf("📌 %s · %d/%d", truncate(p.Current, limit), p.Done, p.Total)
		}
		return fmt.Sprintf("📌 %d/%d", p.Done, p.Total)
	case "TaskCreate":
		var inp struct {
			Subject string `json:"subject"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Subject != "" {
			return fmt.Sprintf("📌 + %s", truncate(inp.Subject, limit))
		}
		return "📌 +"
	case "TaskUpdate":
		var inp struct {
			TaskID     string `json:"taskId"`
			Status     string `json:"status"`
			Subject    string `json:"subject"`
			ActiveForm string `json:"activeForm"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		id := inp.TaskID
		if id == "" {
			id = "?"
		}
		switch inp.Status {
		case "in_progress":
			return fmt.Sprintf("📌 #%s ▶", id)
		case "completed":
			return fmt.Sprintf("📌 #%s ✓", id)
		case "deleted":
			return fmt.Sprintf("📌 #%s ✕", id)
		case "pending":
			return fmt.Sprintf("📌 #%s ⏸", id)
		}
		if inp.Subject != "" || inp.ActiveForm != "" {
			return fmt.Sprintf("📌 #%s ✎", id)
		}
		return fmt.Sprintf("📌 #%s", id)
	case "TaskList":
		return "📌 list"
	case "TaskGet":
		var inp struct {
			TaskID string `json:"taskId"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		id := inp.TaskID
		if id == "" {
			id = "?"
		}
		return fmt.Sprintf("📌 #%s ?", id)
	case "MCP":
		var inp struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		label := inp.Tool
		if inp.Server != "" && inp.Tool != "" {
			label = inp.Server + "." + inp.Tool
		} else if inp.Server != "" {
			label = inp.Server
		}
		if label == "" {
			return "🔌 MCP"
		}
		return fmt.Sprintf("🔌 MCP: %s", label)
	default:
		return fmt.Sprintf("🔧 %s", t.Name)
	}
}

// humanTokens renders a token count compactly: 144630 → "144k", 730 → "730".
func humanTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// formatCompact renders a context-compaction boundary for the chat log.
func formatCompact(c *CompactInfo) string {
	trigger := c.Trigger
	if trigger == "" {
		trigger = "auto"
	}
	return fmt.Sprintf("🗜 Контекст свёрнут: %s→%s токенов (%s)", humanTokens(c.PreTokens), humanTokens(c.PostTokens), trigger)
}

func formatRateLimit(rl *RateLimitInfo) string {
	typeLabel := ""
	switch rl.RateLimitType {
	case "five_hour":
		typeLabel = "5ч"
	case "weekly", "seven_day":
		typeLabel = "нед"
	default:
		typeLabel = rl.RateLimitType
	}
	remaining := ""
	if rl.ResetsAt > 0 {
		d := time.Until(time.Unix(rl.ResetsAt, 0))
		if d > 0 {
			remaining = " " + fmtutil.Duration(d)
		}
	}
	switch rl.Status {
	case "throttled", "rejected":
		s := fmt.Sprintf("🚫 Лимит (%s)%s", typeLabel, remaining)
		if rl.IsUsingOverage {
			s += " (overage)"
		}
		return s
	case "allowed_warning":
		pct := int(rl.Utilization * 100)
		return fmt.Sprintf("⚠️ Лимит (%s) %d%%%s", typeLabel, pct, remaining)
	default:
		return fmt.Sprintf("⏱ Лимит (%s) %s%s", typeLabel, rl.Status, remaining)
	}
}

func truncate(s string, n int) string {
	s = pathutil.TildePathsInText(s)
	if n <= 0 {
		if s == "" {
			return s
		}
		return "…"
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}

func oneLinePreview(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RunOptions configures a CLI invocation.
type RunOptions struct {
	Prompt                    string
	SessionID                 string // empty = new session
	CWD                       string // working directory
	Sandbox                   string // "on" = CLI defaults, "off" = unrestricted
	Model                     string // model override
	Effort                    string // reasoning effort: low | medium | high (claude also: max; codex also: xhigh)
	ContextWindowHint         int    // last known context window for progress usage that only reports used tokens
	AppendSystemPrompt        string // appended to default system prompt
	ClaudeTTY                 bool   // run Claude through klax tty instead of claude -p directly
	SuppressNarrationProgress bool   // keep final-answer text buffered instead of streaming it as narration
}

// ModelUsageInfo captures context window usage from a run.
type ModelUsageInfo struct {
	Model         string
	ContextWindow int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreation int
	ContextUsed   int
}

// RunResult is the final result of a CLI invocation.
type RunResult struct {
	SessionID string
	Text      string
	Usage     ModelUsageInfo
	RateLimit *RateLimitInfo
	Error     error
}

// ProgressKind discriminates progress events surfaced during a run.
type ProgressKind string

const (
	// ProgressKindTool is a tool invocation label ("⚙️ Bash: ls ~").
	ProgressKindTool ProgressKind = "tool"
	// ProgressKindNarration is an assistant text block that turned out not
	// to be the final answer (another text block came after it). Frontends
	// should render it distinctly from the final answer body.
	ProgressKindNarration ProgressKind = "narration"
	// ProgressKindContext is a usage-only update. Chat frontends can reflect it
	// in an in-flight turn indicator without adding a timeline block.
	ProgressKindContext ProgressKind = "context"
)

// ProgressEvent is a single streamed progress update.
type ProgressEvent struct {
	Kind ProgressKind
	Text string
	// Tool is the structured tool invocation behind a ProgressKindTool event
	// whose label came from a real ToolUse. It is nil for synthesized
	// tool-kind events (rate-limit, unknown, error, compact) and for narration.
	// Text already holds the default-width label (ToolUse.String); a frontend
	// with more room than Telegram re-renders this via ToolUse.Preview at a
	// wider limit instead of consuming the clipped Text.
	Tool *ToolUse
	// Usage is the latest context snapshot known at this progress point.
	Usage ModelUsageInfo
}

// ProgressFunc is called with human-readable progress updates.
//
// Callbacks run while the runner holds narrationBuffer.mu, so the function
// must be quick and non-reentrant — anything heavier than append-to-slice
// (network, formatting beyond cheap string ops, calling back into the
// runner) belongs downstream behind a non-blocking mailbox. Holding mu
// during the callback is intentional: it preserves the ordering invariant
// that idle-timer narration emits and read-loop tool emits never
// interleave at the consumer.
type ProgressFunc func(ev ProgressEvent)

// Runner executes an AI backend and tracks state.
type Runner struct {
	mu      sync.Mutex
	busy    bool
	current ToolUse
	startAt time.Time
	toolAt  time.Time
}

func New() *Runner {
	return &Runner{}
}

func (r *Runner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

// Status returns current tool, time since tool started, and total run time.
func (r *Runner) Status() (tool ToolUse, toolElapsed, totalElapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := time.Since(r.startAt)
	var te time.Duration
	if r.current.Name != "" {
		te = time.Since(r.toolAt)
	}
	return r.current, te, total
}

// narrationBuffer accumulates per-run assistant text and decides when to
// emit it as ProgressKindNarration: look-ahead (on a paragraph boundary
// with a tail as "more is coming" proof) or idle (timer fires while the
// backend is still alive). All ProgressEvents from a Run — narration and
// tool — flow through here; mu serializes the read-loop goroutine, the idle
// timer goroutine, and onProgress. After drain() any further AfterFunc
// callback is a no-op.
type narrationBuffer struct {
	mu         sync.Mutex
	text       string
	timer      *time.Timer
	onProgress ProgressFunc
	suppress   bool
	closed     bool
	// idleGen identifies the currently-armed idle timer. armIdleLocked
	// bumps it; the AfterFunc closure captures the value and idleFire
	// bails on mismatch — prevents a callback that was blocked on mu
	// from emitting after a fresh append re-armed the timer.
	idleGen uint64
	// pendingJoin defers a paragraph break to the next non-empty append.
	// Set by markBlockBoundary on a text-block end signal; consumed under
	// mu on the next append. Harmless if no append follows.
	pendingJoin bool
}

func newNarrationBuffer(fn ProgressFunc, suppress bool) *narrationBuffer {
	return &narrationBuffer{onProgress: fn, suppress: suppress}
}

// append adds a fragment. Returns true on success so callers can maintain
// sawText. isDelta=true means raw concat (token stream — the model's own
// newlines carry structure). isDelta=false means paragraph-join with
// "\n\n" against any existing buffer. A deferred block boundary (see
// markBlockBoundary) takes precedence over either mode.
func (b *narrationBuffer) append(text string, isDelta bool) bool {
	if !isDelta {
		text = strings.Trim(text, "\n")
	}
	if text == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	// Consume any deferred block break first so the next content lands in
	// its own paragraph regardless of mode.
	if b.pendingJoin && b.text != "" {
		b.text = strings.TrimRight(b.text, "\n") + "\n\n"
	}
	b.pendingJoin = false
	switch {
	case b.text == "":
		b.text = text
	case isDelta:
		b.text += text
	default:
		// Paragraph join: trim trailing newlines from buf so a delta tail
		// doesn't stack with the join into 3-4 consecutive newlines.
		b.text = strings.TrimRight(b.text, "\n") + "\n\n" + text
	}
	b.maybeLookAheadLocked()
	b.armIdleLocked()
	return true
}

// demote flushes the buffer as one narration event and stops the idle
// timer. Called at stream boundaries (tool, rate-limit, unknown, cancel,
// result error) so log order matches stream order.
func (b *narrationBuffer) demote() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if b.text == "" {
		return
	}
	out := b.text
	b.text = ""
	b.emitLocked(ProgressEvent{Kind: ProgressKindNarration, Text: out})
}

// drain returns the buffer without emitting and marks it closed. Called
// at end-of-stream: the returned text becomes RunResult.Text and no
// further emit (including a fired-but-blocked AfterFunc) can reach
// onProgress after this returns — queue.go closes progressCh right
// after Run, so a late send would panic.
func (b *narrationBuffer) drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	out := b.text
	b.text = ""
	return out
}

// emitTool sends a non-narration event under mu so it cannot race with
// an idle fire. Order at boundaries (demote → emitTool) is preserved
// because both take this lock.
func (b *narrationBuffer) emitTool(ev ProgressEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.emitLocked(ev)
}

// markBlockBoundary defers a paragraph break to the next non-empty
// append. No-op if the buffer is empty (nothing to glue to).
func (b *narrationBuffer) markBlockBoundary() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.text == "" {
		return
	}
	b.pendingJoin = true
}

func (b *narrationBuffer) emitLocked(ev ProgressEvent) {
	if b.closed || b.onProgress == nil || (ev.Text == "" && ev.Kind != ProgressKindContext) {
		return
	}
	b.onProgress(ev)
}

// tryCutLocked emits one narration cut on the first paragraph boundary
// past minBody (look-ahead uses narrationFlushMinChars; idle uses 0). The
// "\n\n" it consumes doubles as the log/final separator inserted by
// queue.go's final delivery, so concat(log + "\n\n" + final) reconstructs
// the original text exactly. The tail must be at least
// narrationFlushLookaheadChars to count as "more is coming" proof.
func (b *narrationBuffer) tryCutLocked(minBody int) {
	if minBody+2+narrationFlushLookaheadChars > len(b.text) {
		return
	}
	bodyEnd := findParagraphCut(b.text, minBody)
	if bodyEnd <= 0 {
		return
	}
	tailStart := bodyEnd + 2
	if len(b.text)-tailStart < narrationFlushLookaheadChars {
		return
	}
	// Clone the body so it doesn't anchor the pre-cut backing array in
	// queue.go's logItems — keeps memory linear in the streamed reply.
	body := strings.Clone(b.text[:bodyEnd])
	b.text = b.text[tailStart:]
	b.emitLocked(ProgressEvent{Kind: ProgressKindNarration, Text: body})
}

// findParagraphCut finds the first "\n\n" outside any fenced code block at
// body length >= minBody. Returns the byte index of the first "\n", or -1
// if none qualifies. Stays outside fences so a code block containing a
// blank line is never split across the log/final boundary. Tilde and
// backtick fences are interchangeable here — mixing the two in one
// response is exotic enough that we don't track which character opened
// the fence.
func findParagraphCut(text string, minBody int) int {
	inFence := false
	lineStart := 0
	for {
		rel := strings.IndexByte(text[lineStart:], '\n')
		if rel < 0 {
			return -1
		}
		nl := lineStart + rel
		line := text[lineStart:nl]
		if line == "" && !inFence && nl-1 >= minBody {
			return nl - 1
		}
		// Fence marker: ``` or ~~~ after up to 3 cols of leading indent
		// (CommonMark). Tabs counted as 1 col — strictly a tab is 4 cols
		// and would disqualify, but missing a tab-indented fence on rare
		// mixed-indent output is the worse failure mode.
		trimmed := strings.TrimLeft(line, " \t")
		if leadingIndent := len(line) - len(trimmed); leadingIndent <= 3 {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = !inFence
			}
		}
		lineStart = nl + 1
	}
}

func (b *narrationBuffer) maybeLookAheadLocked() {
	if b.suppress {
		return
	}
	b.tryCutLocked(narrationFlushMinChars)
}

func (b *narrationBuffer) armIdleLocked() {
	if b.suppress {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.idleGen++
	gen := b.idleGen
	b.timer = time.AfterFunc(narrationIdleTimeout, func() { b.idleFire(gen) })
}

func (b *narrationBuffer) idleFire(gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if gen != b.idleGen {
		// Stale callback: a fresh append re-armed the timer.
		return
	}
	// Idle reuses tryCutLocked with minBody=0: commit any completed
	// paragraph if buffered, otherwise wait — never split mid-sentence.
	// End-of-run demotes whatever stays.
	b.tryCutLocked(0)
}

// Run executes the backend with streaming output. Cancelling ctx sends SIGTERM
// to the backend's process group, then SIGKILL after killGracePeriod, and
// closes the stdout pipe so the read loop unblocks even if children still
// hold write-ends (e.g. rust grandchild of the codex npm shim).
func (r *Runner) Run(ctx context.Context, backend Backend, opts RunOptions, onProgress ProgressFunc) RunResult {
	r.mu.Lock()
	r.busy = true
	r.startAt = time.Now()
	r.current = ToolUse{}
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.busy = false
		r.current = ToolUse{}
		r.mu.Unlock()
	}()

	cmd, err := backend.BuildCmd(opts)
	if err != nil {
		return RunResult{Error: err}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{Error: fmt.Errorf("pipe: %w", err)}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return RunResult{Error: fmt.Errorf("start: %w", err)}
	}

	stopWatcher := watchCancel(ctx, cmd.Process.Pid, stdout)
	defer stopWatcher()

	sessionID := opts.SessionID
	var model string
	var textParts []string
	var usage ModelUsageInfo
	if opts.ContextWindowHint > 0 {
		usage.ContextWindow = opts.ContextWindowHint
	}
	var rateLimit *RateLimitInfo

	// sawText tracks whether the stream emitted any assistant text block for
	// this turn. When set, we ignore the redundant `result.text` field (which
	// Claude echoes from the final assistant block) to avoid duplicating the
	// tail of the answer in RunResult.Text.
	var sawText bool

	// buf accumulates consecutive assistant text blocks until a
	// chronological boundary arrives — a tool invocation, an unknown
	// progress item, a rate-limit event, or end of stream. On a boundary
	// the accumulated block is demoted to one or more narration progress
	// events (preserving order in the log); at end of stream the remainder
	// is promoted to the final answer body. While text is accumulating,
	// look-ahead / idle policy inside narrationBuffer may emit chunks
	// early so a long answer feels alive rather than silent — but only
	// when a tail is held back as proof more is coming, or after a long
	// quiet stretch with the backend still alive.
	buf := newNarrationBuffer(onProgress, opts.SuppressNarrationProgress)
	mergeUsage := func(next ModelUsageInfo) bool {
		changed := false
		if next.Model != "" && usage.Model != next.Model {
			usage.Model = next.Model
			changed = true
		}
		if next.ContextWindow > 0 && usage.ContextWindow != next.ContextWindow {
			usage.ContextWindow = next.ContextWindow
			changed = true
		}
		if next.ContextUsed > 0 && usage.ContextUsed != next.ContextUsed {
			usage.ContextUsed = next.ContextUsed
			changed = true
		}
		if next.InputTokens > 0 && usage.InputTokens != next.InputTokens {
			usage.InputTokens = next.InputTokens
			changed = true
		}
		if next.OutputTokens > 0 && usage.OutputTokens != next.OutputTokens {
			usage.OutputTokens = next.OutputTokens
			changed = true
		}
		if next.CacheRead > 0 && usage.CacheRead != next.CacheRead {
			usage.CacheRead = next.CacheRead
			changed = true
		}
		if next.CacheCreation > 0 && usage.CacheCreation != next.CacheCreation {
			usage.CacheCreation = next.CacheCreation
			changed = true
		}
		return changed
	}
	emitUsage := func() {
		if usage.ContextUsed > 0 && usage.ContextWindow > 0 {
			buf.emitTool(ProgressEvent{Kind: ProgressKindContext, Usage: usage})
		}
	}
	var stopCodexMetaTail context.CancelFunc
	startCodexMetaTail := func(threadID string) {
		if backend.Name() != "codex" || threadID == "" || stopCodexMetaTail != nil {
			return
		}
		tailCtx, cancel := context.WithCancel(ctx)
		stopCodexMetaTail = cancel
		tail := newCodexSessionMetaTail(threadID)
		go func() {
			var lastUsed, lastWindow int
			ticker := time.NewTicker(750 * time.Millisecond)
			defer ticker.Stop()
			poll := func() {
				meta, changed := tail.Poll()
				if !changed {
					return
				}
				window := meta.ContextWindow
				if window == 0 {
					window = opts.ContextWindowHint
				}
				if meta.ContextUsed > 0 && window > 0 && (meta.ContextUsed != lastUsed || window != lastWindow) {
					lastUsed, lastWindow = meta.ContextUsed, window
					buf.emitTool(ProgressEvent{
						Kind: ProgressKindContext,
						Usage: ModelUsageInfo{
							Model:         meta.Model,
							ContextUsed:   meta.ContextUsed,
							ContextWindow: window,
						},
					})
				}
			}
			poll()
			for {
				select {
				case <-tailCtx.Done():
					return
				case <-ticker.C:
					poll()
				}
			}
		}()
	}
	stopCodexMetaTailIfRunning := func() {
		if stopCodexMetaTail != nil {
			stopCodexMetaTail()
			stopCodexMetaTail = nil
		}
	}
	defer stopCodexMetaTailIfRunning()

	// A resumed codex run reuses the session id we passed and may never re-emit a system
	// event carrying it, so seed the live context tail from the id we already know. New runs
	// pass "" (no-op), and a later EventSystem is a no-op via startCodexMetaTail's guard.
	startCodexMetaTail(sessionID)

	// resultErr holds a failure the backend reported mid-stream (an errored
	// `result`/error event). We record it but keep reading so the stream still
	// drains to EOF and every run reaches the single cmd.Wait()/cleanup path
	// below — returning early would skip Wait and risk orphaning a backend that
	// is still writing. streamErr is how the read loop exited (io.EOF on a clean
	// end, or a real read failure).
	var resultErr error
	var streamErr error

	// Read events with a bufio.Reader, not a bufio.Scanner: codex can emit a
	// single JSON event far larger than any fixed scanner buffer (a file_read
	// with big contents, a command's aggregated output). A scanner that hit its
	// cap returned ErrTooLong and the loop exited; stdout then went undrained,
	// the backend blocked on a full stdout pipe, and the cmd.Wait() below hung
	// forever — wedging the whole run. readEventLine consumes arbitrarily long
	// lines so the stream always drains to EOF and the backend can exit.
	reader := bufio.NewReaderSize(stdout, 64*1024)
	for {
		line, readErr := readEventLine(reader)
		if len(line) == 0 {
			if readErr != nil {
				streamErr = readErr
				if readErr != io.EOF {
					log.Printf("runner: %s stdout read error: %v", backend.Name(), readErr)
				}
				break
			}
			continue
		}

		events, ok := backend.ParseEvent(line)
		if !ok {
			continue
		}

		for _, ev := range events {
			switch ev.Type {
			case EventSystem:
				if ev.SessionID != "" {
					sessionID = ev.SessionID
					startCodexMetaTail(sessionID)
				}
				if ev.Model != "" {
					model = ev.Model
				}
				if ev.RateLimit != nil {
					rateLimit = ev.RateLimit
					if ev.RateLimit.Status != "allowed" {
						// Flush pending first so the rate-limit line
						// appears in the log AFTER the text that preceded
						// it chronologically.
						if !opts.SuppressNarrationProgress {
							buf.demote()
						}
						buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: formatRateLimit(ev.RateLimit)})
					}
				}

			case EventTool:
				r.mu.Lock()
				r.current = ev.Tool
				r.toolAt = time.Now()
				r.mu.Unlock()
				// A tool call is the chronological boundary between
				// narration and what comes after. Flush pending BEFORE
				// emitting the tool so the log order matches the stream.
				buf.demote()
				tool := ev.Tool
				if mergeUsage(ev.Usage) {
					emitUsage()
				}
				buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: tool.String(), Tool: &tool, Usage: usage})

			case EventText:
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if mergeUsage(ev.Usage) {
					emitUsage()
				}
				if buf.append(ev.Text, false) {
					sawText = true
				}

			case EventTextDelta:
				// Raw-concat token stream from --include-partial-messages;
				// look-ahead inside buf emits paragraphs as they form.
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if buf.append(ev.Text, true) {
					sawText = true
				}

			case EventTextBoundary:
				// End of a streamed text block — defer a paragraph break
				// before the next block's deltas start.
				buf.markBlockBoundary()

			case EventIntermediate:
				// Codex emits one `agent_message` per assistant text
				// fragment within a turn. Accumulate; the boundary is
				// the next tool/end-of-stream, same as Claude.
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if buf.append(ev.Text, false) {
					sawText = true
				}

			case EventUnknown:
				if ev.Text != "" {
					if !opts.SuppressNarrationProgress {
						buf.demote()
					}
					buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: fmt.Sprintf("❓ %s", ev.Text)})
				}

			case EventError:
				if ev.Text != "" {
					if !opts.SuppressNarrationProgress {
						buf.demote()
					}
					buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: fmt.Sprintf("❌ %s", ev.Text)})
				}

			case EventCompact:
				if ev.Compact != nil {
					// Like a rate-limit line: an out-of-band event, not a content
					// boundary. Flush pending narration first so the log reads in
					// stream order — but never in suppressed mode, where the
					// buffer holds the final answer and demoting it here would
					// silently drop everything written before the compaction.
					if !opts.SuppressNarrationProgress {
						buf.demote()
					}
					buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: formatCompact(ev.Compact)})
				}

			case EventResult:
				stopCodexMetaTailIfRunning()
				// `result` marks the end of one agent-loop iteration, not
				// the end of the run. claude -p --output-format stream-json
				// keeps the loop alive while background tasks (run_in_background,
				// Monitor) are pending and injects their completions as
				// <task-notification> user messages → another assistant turn
				// → another `result`. We let those subsequent events flow
				// through normally; the run ends when the CLI exits and the
				// read loop hits EOF.
				if ev.Error != "" {
					// Record the failure (first one wins) but keep reading
					// to EOF so the cmd.Wait()/cleanup path below still runs;
					// returning here would skip Wait and could orphan a
					// backend that is still writing. Demote preserves any
					// accumulated text as narration so the user still sees
					// what the model said before the error — the queue error
					// branch renders logItems alongside the error marker.
					if resultErr == nil {
						resultErr = fmt.Errorf("%s: %s", backend.Name(), ev.Error)
					}
					buf.demote()
					continue
				}
				// Trust result.text only when no assistant text blocks
				// were seen (older Claude streams, or backends that do
				// not emit per-block text). Otherwise result.text just
				// duplicates the block already held in pending.
				if ev.Text != "" && !sawText {
					textParts = append(textParts, ev.Text)
				}
				// Merge usage from result event. Do not emit a context progress update here:
				// result is immediately followed by final delivery, where the timeline prints
				// the end-of-turn context once. Updating the dots right before removing them
				// creates a visible but meaningless flicker.
				mergeUsage(ev.Usage)
			}
		}
	}

	// A real stdout read failure (not a clean EOF, and not ctx cancellation —
	// watchCancel already handles that) means we can no longer drain the pipe.
	// Force the backend down so cmd.Wait() cannot hang on a child blocked
	// writing into a now-unread pipe — the same wedge class as the
	// oversized-line bug, reached via a different exit.
	if streamErr != nil && streamErr != io.EOF && ctx.Err() == nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = stdout.Close()
	}

	waitErr := cmd.Wait()

	// Cancellation path: whatever is in pending is not a sanctioned
	// final answer, but it may be substantial narration the user was
	// about to see. Demote it so the error branch in queue.go can surface
	// it via logItems alongside the error marker. `Error != nil` still
	// signals the caller that the turn did not complete, so session
	// counters / model state do not advance.
	if ctx.Err() != nil {
		buf.demote()
		_ = buf.drain()
		return RunResult{
			SessionID: sessionID,
			Error:     fmt.Errorf("%s: %w", backend.Name(), ctx.Err()),
		}
	}

	// Backend reported a failed turn mid-stream. Same narration-preserving
	// treatment as cancellation: demote pending into the log, return the error.
	if resultErr != nil {
		buf.demote()
		_ = buf.drain()
		return RunResult{SessionID: sessionID, Error: resultErr}
	}

	// stdout read failed partway through — the turn is incomplete, so surface
	// the read failure rather than silently treating a truncated stream as a
	// finished answer.
	if streamErr != nil && streamErr != io.EOF {
		buf.demote()
		_ = buf.drain()
		return RunResult{
			SessionID: sessionID,
			Error:     fmt.Errorf("%s: stdout read error: %w", backend.Name(), streamErr),
		}
	}

	// The run completed normally — whatever stayed in pending past the
	// last tool boundary is the actual reply. drain also closes the
	// buffer so any in-flight idle-timer callback becomes a no-op before
	// queue.go closes progressCh.
	if remainder := buf.drain(); remainder != "" {
		textParts = append(textParts, remainder)
	}

	if usage.Model == "" {
		usage.Model = model
	}

	// For codex: read model, effective context window, and the latest turn's
	// prompt size from the local session file. On resumed runs, codex may not
	// re-emit thread.started, so fall back to the SessionID we already passed.
	if backend.Name() == "codex" && sessionID != "" {
		if m, cw, cu := ReadCodexSessionMeta(sessionID); m != "" || cw > 0 || cu > 0 {
			if usage.Model == "" {
				usage.Model = m
			}
			if usage.ContextWindow == 0 {
				usage.ContextWindow = cw
			}
			if cu > 0 {
				usage.ContextUsed = cu
			}
		}
	}

	text := strings.Join(textParts, "\n")
	result := RunResult{
		SessionID: sessionID,
		Text:      text,
		Usage:     usage,
		RateLimit: rateLimit,
	}
	if text == "" && waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			result.Error = fmt.Errorf("%s: %s", backend.Name(), stderr)
		} else {
			result.Error = fmt.Errorf("%s exited: %w", backend.Name(), waitErr)
		}
	}
	return result
}

// maxEventLine caps a single backend JSON event line. Lines up to this size are
// delivered intact; a longer line is truncated (and will simply fail to parse)
// but the stream keeps draining. 16 MiB comfortably covers any legitimate codex
// event (large file_read contents, command aggregated output) while bounding
// memory if a backend ever emits something pathological.
const maxEventLine = 16 * 1024 * 1024

// readEventLine reads one '\n'-terminated line from r, capped at maxEventLine
// bytes. Unlike bufio.Scanner it never gives up on an over-long line: bytes past
// the cap are read and discarded up to the newline, so the caller always
// consumes the whole stream and the backend can never wedge on a full pipe. The
// trailing newline (and CR) is stripped. err is io.EOF at end of stream and may
// accompany a final unterminated line.
func readEventLine(r *bufio.Reader) (line []byte, err error) {
	for {
		var chunk []byte
		chunk, err = r.ReadSlice('\n')
		if room := maxEventLine - len(line); room > 0 {
			if len(chunk) > room {
				line = append(line, chunk[:room]...)
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			err = nil
			continue
		}
		break
	}
	// Strip the line terminator to match the previous scanner semantics.
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
		if n = len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
	}
	return line, err
}

// watchCancel escalates ctx cancellation into process-group termination. The
// backend command is launched with Setpgid, so the child's pid equals the
// pgid and we can signal every descendant (e.g. the rust grandchild behind
// the codex npm shim) with a single Kill(-pid).
//
// On cancel it sends SIGTERM, waits killGracePeriod, then SIGKILL and closes
// stdout so the read loop unblocks even if a grandchild still holds a
// write-end of the pipe.
//
// The returned stop function must be called when the Run completes normally
// to release the watcher goroutine.
func watchCancel(ctx context.Context, pid int, stdout io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		// Signal the entire process group. Negative pid in Kill targets the
		// group whose leader has |pid|. Errors here are best-effort: the
		// process may have already exited.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(killGracePeriod):
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		// Closing stdout unblocks the read loop if a grandchild inherited and
		// still holds the write-end after the shim exited.
		if stdout != nil {
			if err := stdout.Close(); err != nil {
				log.Printf("runner: stdout close after cancel failed: %v", err)
			}
		}
	}()
	return func() { close(done) }
}
