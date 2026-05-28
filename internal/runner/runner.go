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

func (t ToolUse) String() string {
	switch t.Name {
	case "Bash":
		var inp struct{ Command string }
		json.Unmarshal([]byte(t.Input), &inp)
		return fmt.Sprintf("⚙️ Bash: `%s`", truncate(oneLinePreview(inp.Command), toolPreviewLimit))
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
		return fmt.Sprintf("🌐 Fetch: %s", truncate(inp.URL, toolPreviewLimit))
	case "WebSearch":
		var inp struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Query != "" {
			return fmt.Sprintf("🔎 Search: %s", truncate(inp.Query, toolPreviewLimit))
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
			return fmt.Sprintf("🤖 Task: %s", truncate(inp.Description, toolPreviewLimit))
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
			return fmt.Sprintf("📌 %s · %d/%d", truncate(p.Current, toolPreviewLimit), p.Done, p.Total)
		}
		return fmt.Sprintf("📌 %d/%d", p.Done, p.Total)
	case "TaskCreate":
		var inp struct {
			Subject string `json:"subject"`
		}
		json.Unmarshal([]byte(t.Input), &inp)
		if inp.Subject != "" {
			return fmt.Sprintf("📌 + %s", truncate(inp.Subject, toolPreviewLimit))
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
	Prompt             string
	SessionID          string // empty = new session
	CWD                string // working directory
	Sandbox            string // "on" = CLI defaults, "off" = unrestricted
	Model              string // model override
	Effort             string // reasoning effort: low | medium | high (claude also: max; codex also: xhigh)
	AppendSystemPrompt string // appended to default system prompt
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
)

// ProgressEvent is a single streamed progress update.
type ProgressEvent struct {
	Kind ProgressKind
	Text string
}

// ProgressFunc is called with human-readable progress updates.
//
// Callbacks run while the runner holds narrationBuffer.mu, so the function
// must be quick and non-reentrant — anything heavier than append-to-slice
// (network, formatting beyond cheap string ops, calling back into the
// runner) belongs downstream behind a non-blocking mailbox. Holding mu
// during the callback is intentional: it preserves the ordering invariant
// that idle-timer narration emits and scanner-goroutine tool emits never
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
// tool — flow through here; mu serializes the scanner goroutine, the idle
// timer goroutine, and onProgress. After drain() any further AfterFunc
// callback is a no-op.
type narrationBuffer struct {
	mu         sync.Mutex
	text       string
	timer      *time.Timer
	onProgress ProgressFunc
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

func newNarrationBuffer(fn ProgressFunc) *narrationBuffer {
	return &narrationBuffer{onProgress: fn}
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
	if b.closed || b.onProgress == nil || ev.Text == "" {
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
	b.tryCutLocked(narrationFlushMinChars)
}

func (b *narrationBuffer) armIdleLocked() {
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
// closes the stdout pipe so the scanner loop unblocks even if children still
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
	buf := newNarrationBuffer(onProgress)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
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
						buf.demote()
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
				buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: ev.Tool.String()})
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}

			case EventText:
				r.mu.Lock()
				r.current = ToolUse{}
				r.mu.Unlock()
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
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
					buf.demote()
					buf.emitTool(ProgressEvent{Kind: ProgressKindTool, Text: fmt.Sprintf("❓ %s", ev.Text)})
				}

			case EventResult:
				// `result` marks the end of one agent-loop iteration, not
				// the end of the run. claude -p --output-format stream-json
				// keeps the loop alive while background tasks (run_in_background,
				// Monitor) are pending and injects their completions as
				// <task-notification> user messages → another assistant turn
				// → another `result`. We let those subsequent events flow
				// through normally; the run ends when the CLI exits and the
				// scanner hits EOF.
				if ev.Error != "" {
					// Preserve any accumulated text as narration so the
					// user still sees what the model said before the
					// error — the queue error branch renders logItems
					// alongside the error marker.
					buf.demote()
					_ = buf.drain()
					return RunResult{
						SessionID: sessionID,
						Error:     fmt.Errorf("%s: %s", backend.Name(), ev.Error),
					}
				}
				// Trust result.text only when no assistant text blocks
				// were seen (older Claude streams, or backends that do
				// not emit per-block text). Otherwise result.text just
				// duplicates the block already held in pending.
				if ev.Text != "" && !sawText {
					textParts = append(textParts, ev.Text)
				}
				// Merge usage from result event.
				if ev.Usage.Model != "" {
					usage.Model = ev.Usage.Model
				}
				if ev.Usage.ContextWindow > 0 {
					usage.ContextWindow = ev.Usage.ContextWindow
				}
				if ev.Usage.ContextUsed > 0 {
					usage.ContextUsed = ev.Usage.ContextUsed
				}
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.CacheRead > 0 {
					usage.CacheRead = ev.Usage.CacheRead
				}
				if ev.Usage.CacheCreation > 0 {
					usage.CacheCreation = ev.Usage.CacheCreation
				}
			}
		}
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

// watchCancel escalates ctx cancellation into process-group termination. The
// backend command is launched with Setpgid, so the child's pid equals the
// pgid and we can signal every descendant (e.g. the rust grandchild behind
// the codex npm shim) with a single Kill(-pid).
//
// On cancel it sends SIGTERM, waits killGracePeriod, then SIGKILL and closes
// stdout so the scanner loop unblocks even if a grandchild still holds a
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
		// Closing stdout unblocks the scanner if a grandchild inherited and
		// still holds the write-end after the shim exited.
		if stdout != nil {
			if err := stdout.Close(); err != nil {
				log.Printf("runner: stdout close after cancel failed: %v", err)
			}
		}
	}()
	return func() { close(done) }
}
