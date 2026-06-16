package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestToolUseStringAppliesTildeBeforeTruncate(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	cmd := `cd "` + home + `/very/long/path/with/many/segments/for/testing/that/keeps/going/and/going/through/even/more/directories/after/the/new/preview/limit" && echo done`
	got := ToolUse{
		Name:  "Bash",
		Input: `{"command":"` + strings.ReplaceAll(cmd, `"`, `\"`) + `"}`,
	}.String()

	if !strings.Contains(got, "~/very/long/path") {
		t.Fatalf("expected tilde path in %q", got)
	}
	if strings.Contains(got, home) {
		t.Fatalf("home path should be compacted before truncation in %q", got)
	}
	if !strings.HasSuffix(got, "…`") {
		t.Fatalf("expected truncated bash preview in %q", got)
	}
}

func TestToolUseStringCompactsMultilineBashPreview(t *testing.T) {
	got := ToolUse{
		Name:  "Bash",
		Input: `{"command":"set -e\nrm -f /tmp/known_hosts\n\necho done"}`,
	}.String()

	if strings.Contains(got, "\n") {
		t.Fatalf("expected one-line bash preview, got %q", got)
	}
	if !strings.Contains(got, "set -e rm -f /tmp/known_hosts echo done") {
		t.Fatalf("unexpected compacted bash preview: %q", got)
	}
}

func TestTruncatePreservesUTF8(t *testing.T) {
	s := `"335-й стрелковый полк" "58-я стрелковая дивизия"`

	cut := 0
	for i := 1; i < len(s); i++ {
		if !utf8.ValidString(s[:i]) {
			cut = i
			break
		}
	}
	if cut == 0 {
		t.Fatal("test string did not produce a mid-rune byte boundary")
	}

	got := truncate(s, cut)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate returned invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("truncate introduced replacement rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncated string to end with ellipsis: %q", got)
	}
}

func TestToolUseString_PlanAndTask(t *testing.T) {
	cases := []struct {
		name string
		tool ToolUse
		want string
	}{
		{
			name: "Plan empty payload",
			tool: ToolUse{Name: "Plan", Input: ""},
			want: "📌 Plan",
		},
		{
			name: "Plan total=0 in JSON",
			tool: ToolUse{Name: "Plan", Input: `{"done":0,"total":0}`},
			want: "📌 Plan",
		},
		{
			name: "Plan in progress with current",
			tool: ToolUse{Name: "Plan", Input: `{"done":1,"total":4,"current":"Running whoami"}`},
			want: "📌 Running whoami · 1/4",
		},
		{
			name: "Plan in progress without current",
			tool: ToolUse{Name: "Plan", Input: `{"done":2,"total":4}`},
			want: "📌 2/4",
		},
		{
			name: "Plan complete",
			tool: ToolUse{Name: "Plan", Input: `{"done":4,"total":4}`},
			want: "📌 ✓ 4/4",
		},
		{
			name: "TaskCreate with subject",
			tool: ToolUse{Name: "TaskCreate", Input: `{"subject":"uptime","description":"Run uptime","activeForm":"Running uptime"}`},
			want: "📌 + uptime",
		},
		{
			name: "TaskCreate without subject",
			tool: ToolUse{Name: "TaskCreate", Input: `{}`},
			want: "📌 +",
		},
		{
			name: "TaskUpdate in_progress",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"1","status":"in_progress"}`},
			want: "📌 #1 ▶",
		},
		{
			name: "TaskUpdate completed",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"2","status":"completed"}`},
			want: "📌 #2 ✓",
		},
		{
			name: "TaskUpdate deleted",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"3","status":"deleted"}`},
			want: "📌 #3 ✕",
		},
		{
			name: "TaskUpdate pending",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"4","status":"pending"}`},
			want: "📌 #4 ⏸",
		},
		{
			name: "TaskUpdate subject edit without status",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"5","subject":"renamed"}`},
			want: "📌 #5 ✎",
		},
		{
			name: "TaskUpdate bare id",
			tool: ToolUse{Name: "TaskUpdate", Input: `{"taskId":"6"}`},
			want: "📌 #6",
		},
		{
			name: "TaskList empty",
			tool: ToolUse{Name: "TaskList", Input: `{}`},
			want: "📌 list",
		},
		{
			name: "TaskGet",
			tool: ToolUse{Name: "TaskGet", Input: `{"taskId":"7"}`},
			want: "📌 #7 ?",
		},
		{
			name: "Task with description",
			tool: ToolUse{Name: "Task", Input: `{"description":"Refactor login flow","prompt":"...long..."}`},
			want: "🤖 Task: Refactor login flow",
		},
		{
			name: "Task without description",
			tool: ToolUse{Name: "Task", Input: `{}`},
			want: "🤖 Task",
		},
		{
			name: "unknown tool falls to wrench default",
			tool: ToolUse{Name: "SomeNewTool", Input: `{}`},
			want: "🔧 SomeNewTool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tool.String()
			if got != tc.want {
				t.Fatalf("ToolUse.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestToolPreviewLimit locks the dual-width contract: String() clips at the
// narrow Telegram default while Preview(UIToolPreviewLimit) shows far more, so
// the web UI keeps long commands the chat path has to cut. UIToolPreviewLimit
// must be well above toolPreviewLimit or the UI gains nothing.
func TestToolPreviewLimit(t *testing.T) {
	if UIToolPreviewLimit <= toolPreviewLimit {
		t.Fatalf("UIToolPreviewLimit (%d) must exceed toolPreviewLimit (%d)", UIToolPreviewLimit, toolPreviewLimit)
	}
	cmd := strings.Repeat("x", UIToolPreviewLimit+50)
	tool := ToolUse{Name: "Bash", Input: `{"command":"` + cmd + `"}`}

	narrow := tool.String()
	if narrow != tool.Preview(toolPreviewLimit) {
		t.Fatalf("String() must equal Preview(toolPreviewLimit)")
	}
	if !strings.Contains(narrow, "…") {
		t.Fatalf("narrow label not truncated: %q", narrow)
	}
	if !strings.Contains(narrow, strings.Repeat("x", toolPreviewLimit)) ||
		strings.Contains(narrow, strings.Repeat("x", toolPreviewLimit+1)) {
		t.Fatalf("narrow label not clipped exactly at toolPreviewLimit")
	}

	wide := tool.Preview(UIToolPreviewLimit)
	if utf8.RuneCountInString(wide) <= utf8.RuneCountInString(narrow) {
		t.Fatalf("Preview(UIToolPreviewLimit)=%d runes not wider than String()=%d", utf8.RuneCountInString(wide), utf8.RuneCountInString(narrow))
	}
	if !strings.Contains(wide, strings.Repeat("x", UIToolPreviewLimit)) {
		t.Fatalf("wide label dropped command content before the wider limit")
	}
}

func TestClaudePlanInput_Normalize(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want PlanProgress
	}{
		{
			name: "all pending picks first content",
			raw:  `{"todos":[{"content":"uptime","status":"pending","activeForm":"Running uptime"},{"content":"whoami","status":"pending","activeForm":"Running whoami"}]}`,
			want: PlanProgress{Done: 0, Total: 2, Current: "uptime"},
		},
		{
			name: "in_progress wins with activeForm",
			raw:  `{"todos":[{"content":"uptime","status":"completed","activeForm":"Running uptime"},{"content":"whoami","status":"in_progress","activeForm":"Running whoami"}]}`,
			want: PlanProgress{Done: 1, Total: 2, Current: "Running whoami"},
		},
		{
			name: "in_progress falls back to content when activeForm missing",
			raw:  `{"todos":[{"content":"uptime","status":"in_progress"}]}`,
			want: PlanProgress{Done: 0, Total: 1, Current: "uptime"},
		},
		{
			name: "all completed has no current",
			raw:  `{"todos":[{"content":"uptime","status":"completed"},{"content":"whoami","status":"completed"}]}`,
			want: PlanProgress{Done: 2, Total: 2, Current: ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudePlanInput([]byte(tc.raw))
			var p PlanProgress
			if err := unmarshalJSON(got, &p); err != nil {
				t.Fatalf("claudePlanInput produced invalid JSON %q: %v", got, err)
			}
			if p != tc.want {
				t.Fatalf("claudePlanInput → %+v, want %+v (raw output %q)", p, tc.want, got)
			}
		})
	}
}

func TestCodexPlanInput_Normalize(t *testing.T) {
	cases := []struct {
		name  string
		items []codexPlanItem
		want  PlanProgress
	}{
		{
			name:  "all incomplete picks first",
			items: []codexPlanItem{{Text: "uptime"}, {Text: "whoami"}},
			want:  PlanProgress{Done: 0, Total: 2, Current: "uptime"},
		},
		{
			name:  "partial progress",
			items: []codexPlanItem{{Text: "uptime", Completed: true}, {Text: "whoami"}, {Text: "date"}},
			want:  PlanProgress{Done: 1, Total: 3, Current: "whoami"},
		},
		{
			name:  "all completed has no current",
			items: []codexPlanItem{{Text: "uptime", Completed: true}, {Text: "whoami", Completed: true}},
			want:  PlanProgress{Done: 2, Total: 2, Current: ""},
		},
		{
			name:  "empty items returns empty string",
			items: nil,
			want:  PlanProgress{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexPlanInput(tc.items)
			if got == "" {
				if tc.want != (PlanProgress{}) {
					t.Fatalf("empty output but want %+v", tc.want)
				}
				return
			}
			var p PlanProgress
			if err := unmarshalJSON(got, &p); err != nil {
				t.Fatalf("codexPlanInput produced invalid JSON %q: %v", got, err)
			}
			if p != tc.want {
				t.Fatalf("codexPlanInput → %+v, want %+v (raw output %q)", p, tc.want, got)
			}
		})
	}
}

func unmarshalJSON(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// scriptBackend runs an arbitrary shell command. Used to simulate the
// codex npm-shim → rust-grandchild topology in process-lifecycle tests
// without depending on a real backend binary. parseAsIntermediate treats
// every stdout line as a codex-style "intermediate" thinking update so we
// can test that cancellation does not let a partial thought leak out as
// a successful answer.
type scriptBackend struct {
	shellCmd            string
	parseAsIntermediate bool
}

func (b *scriptBackend) Name() string { return "script" }

func (b *scriptBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.shellCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *scriptBackend) ParseEvent(line []byte) ([]Event, bool) {
	if b.parseAsIntermediate {
		return []Event{{Type: EventIntermediate, Text: string(line)}}, true
	}
	return nil, false
}

// collectProgress builds a ProgressFunc that records every event so tests
// can assert both the final answer body and the demoted narration/tool log.
type progressRecorder struct {
	events []ProgressEvent
}

func (p *progressRecorder) callback() ProgressFunc {
	return func(ev ProgressEvent) {
		p.events = append(p.events, ev)
	}
}

func (p *progressRecorder) narrationTexts() []string {
	var out []string
	for _, ev := range p.events {
		if ev.Kind == ProgressKindNarration {
			out = append(out, ev.Text)
		}
	}
	return out
}

// kindPairs returns (kind, text) for every progress event in stream order
// so tests can assert the full interleaving of tool and narration entries.
func (p *progressRecorder) kindPairs() [][2]string {
	out := make([][2]string, 0, len(p.events))
	for _, ev := range p.events {
		out = append(out, [2]string{string(ev.Kind), ev.Text})
	}
	return out
}

func TestRunCancelKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "grandchild.pid")
	// Parent sh spawns a grandchild sleep (mirrors npm-shim → rust). Parent
	// then waits. A naive Process.Kill on the shim would leave the sleep
	// orphaned — which is exactly the bug fix tested here.
	script := fmt.Sprintf(`sleep 60 & echo $! > %s; wait`, pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New()
	done := make(chan RunResult, 1)
	go func() {
		done <- r.Run(ctx, &scriptBackend{shellCmd: script}, RunOptions{}, nil)
	}()

	var gcPid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && p > 0 {
				gcPid = p
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if gcPid == 0 {
		t.Fatal("grandchild pid file never populated")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(killGracePeriod + 5*time.Second):
		t.Fatalf("Run did not return after cancel (grandchild pid %d)", gcPid)
	}

	// Grandchild must be gone: Kill(pid, 0) returns ESRCH for dead pids.
	// Poll briefly — the kernel may take a tick to reap after SIGKILL.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(gcPid, 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("grandchild sleep pid %d still alive after cancel", gcPid)
}

// TestClaudeStreamDemotesIntermediatesToNarration encodes the delivery
// contract for Claude streams with multiple text blocks around tool calls:
//
//  1. Narration must appear in the log BEFORE the tool that followed it
//     chronologically (not after), so the log reads in stream order.
//  2. Consecutive text blocks without an intervening tool are a single
//     logical reply — they accumulate, do not split into narration + tail.
//  3. Only text that comes after the last tool boundary becomes the final
//     answer body.
func TestClaudeStreamDemotesIntermediatesToNarration(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		// Outer newlines must be trimmed (both trailing and leading) —
		// but interior \n\n is preserved so markdown rendering keeps
		// paragraph breaks, lists, code fences intact.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"before tool\n"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/x"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"проверка 2\n\nKLODIN"}]}}`,
		// Second text block without an intervening tool — must merge with
		// the previous into one narration / final answer, not split.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"after tool"}]}}`,
		`{"type":"result","session_id":"s1","result":"after tool","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	// "проверка 2 — середина\n\nKLODIN" and "after tool" arrived back-to-
	// back (no tool between them), so they are one logical reply joined
	// by a paragraph break.
	wantText := "проверка 2\n\nKLODIN\n\nafter tool"
	if res.Text != wantText {
		t.Fatalf("final body: got %q, want %q", res.Text, wantText)
	}
	wantOrder := [][2]string{
		{"narration", "before tool"},
		{"tool", "📖 Read: /tmp/x"},
	}
	got := rec.kindPairs()
	if len(got) != len(wantOrder) {
		t.Fatalf("progress events = %v, want %v", got, wantOrder)
	}
	for i, w := range wantOrder {
		if got[i] != w {
			t.Fatalf("progress[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
	if res.SessionID != "s1" {
		t.Fatalf("expected session s1, got %q", res.SessionID)
	}
}

// TestClaudeStreamOrdersNarrationBeforeFollowingTool exercises the
// chronology of narration vs tool entries: A → Read → B → Write → C →
// result. Before the fix, narration was recorded AFTER the tool that
// arrived next (A ended up in the log after Read instead of before it).
// This asserts the full interleaving so that regression resurfaces
// immediately if the boundary handling regresses.
func TestClaudeStreamOrdersNarrationBeforeFollowingTool(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"A"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/r"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"B"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/w"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"C"}]}}`,
		`{"type":"result","session_id":"s1","result":"C","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if res.Text != "C" {
		t.Fatalf("final body: got %q, want %q", res.Text, "C")
	}
	want := [][2]string{
		{"narration", "A"},
		{"tool", "📖 Read: /tmp/r"},
		{"narration", "B"},
		{"tool", "📝 Write: /tmp/w"},
	}
	got := rec.kindPairs()
	if len(got) != len(want) {
		t.Fatalf("progress events = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("progress[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
}

// TestClaudeStreamMultiTurnAgentLoop asserts that events arriving AFTER a
// `result` are kept, not dropped. claude -p --output-format stream-json
// emits one `result` per agent-loop iteration but stays alive while
// background tasks (run_in_background, Monitor) are pending and resumes
// the loop when their completions arrive as <task-notification> user
// messages. Each subsequent turn must surface as narration + tool progress
// in the log; the very last turn's text becomes the final answer body.
func TestClaudeStreamMultiTurnAgentLoop(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		// Turn 1: narration before bg-poller, then result.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Жду поллер"}]}}`,
		`{"type":"result","session_id":"s1","result":"Жду поллер","is_error":false}`,
		// Turn 2 fires after a task-notification: tool, text, result.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/poll.out"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"SSH вернулся"}]}}`,
		`{"type":"result","session_id":"s1","result":"SSH вернулся","is_error":false}`,
		// Turn 3: final mini-report and result, then EOF.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"uname -a"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`,
		`{"type":"result","session_id":"s1","result":"Готово","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	// Final answer = the last turn's trailing text (only it stayed in pending
	// after the last tool boundary; earlier finals were demoted to narration).
	if res.Text != "Готово" {
		t.Fatalf("final body: got %q, want %q", res.Text, "Готово")
	}
	// Earlier turns' final texts must have surfaced as narration before the
	// tool that started the next turn — otherwise the user sees nothing
	// between the first `result` and CLI exit.
	want := [][2]string{
		{"narration", "Жду поллер"},
		{"tool", "📖 Read: /tmp/poll.out"},
		{"narration", "SSH вернулся"},
		{"tool", "⚙️ Bash: `uname -a`"},
	}
	got := rec.kindPairs()
	if len(got) != len(want) {
		t.Fatalf("progress events = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("progress[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
}

// stdinEchoBackend runs a shell command that writes a Claude stream-json
// transcript and then exits. The real ClaudeBackend parser consumes it, so
// this test covers both ParseEvent and the runner loop end-to-end. The
// parser is held by reference so its per-run state (partialDeltaSeen)
// persists across ParseEvent calls — without that, a fixture mixing deltas
// and a final assistant event would re-emit the text on the assistant line.
type stdinEchoBackend struct {
	script string
	parser ClaudeBackend
}

func (b *stdinEchoBackend) Name() string { return "claude" }

func (b *stdinEchoBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *stdinEchoBackend) ParseEvent(line []byte) ([]Event, bool) {
	return b.parser.ParseEvent(line)
}

// TestCodexStreamDemotesIntermediatesToNarration mirrors the Claude
// narration + ordering tests for codex: agent_message before a
// command_execution becomes narration that shows BEFORE the tool in the
// log; agent_message after the last tool becomes the final answer.
func TestCodexStreamDemotesIntermediatesToNarration(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"thread.started","thread_id":"019d0000"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"начинаю"}}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"ls /tmp","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls /tmp","aggregated_output":"a\nb\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"готово"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &codexStreamBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if res.Text != "готово" {
		t.Fatalf("final body must be the post-tool agent_message, got %q", res.Text)
	}
	want := [][2]string{
		{"narration", "начинаю"},
		{"tool", "⚙️ Bash: `ls /tmp`"},
	}
	got := rec.kindPairs()
	if len(got) != len(want) {
		t.Fatalf("progress events = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("progress[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
	if res.SessionID != "019d0000" {
		t.Fatalf("expected thread id propagated, got %q", res.SessionID)
	}
}

func TestCodexSuppressedNarrationKeepsLongReplyInFinalText(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	para1 := strings.Repeat("a", 250)
	para2 := strings.Repeat("b", 150)
	body := para1 + "\n\n" + para2
	jsonBody := strings.ReplaceAll(body, "\n", `\n`)
	lines := []string{
		`{"type":"thread.started","thread_id":"019d0000"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"` + jsonBody + `"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &codexStreamBackend{script: script}, RunOptions{SuppressNarrationProgress: true}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	if narr := rec.narrationTexts(); len(narr) != 0 {
		t.Fatalf("suppressed narration must not emit progress narration, got %v", narr)
	}
	if res.Text != body {
		t.Fatalf("final text must keep the full codex body when narration is suppressed")
	}
}

func TestCodexStreamSurfacesErrorItems(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"thread.started","thread_id":"019d0000"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"rg huge","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"error","message":"tool output exceeded limit","status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"готово"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &codexStreamBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	got := rec.kindPairs()
	want := [][2]string{
		{"tool", "⚙️ Bash: `rg huge`"},
		{"tool", "❌ Codex item error: tool output exceeded limit"},
	}
	if len(got) != len(want) {
		t.Fatalf("progress events = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("progress[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
}

type codexStreamBackend struct {
	script string
}

func (b *codexStreamBackend) Name() string { return "codex" }

func (b *codexStreamBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *codexStreamBackend) ParseEvent(line []byte) ([]Event, bool) {
	return (&CodexBackend{}).ParseEvent(line)
}

func shQuote(args ...string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

func TestRunCancelAfterIntermediateReturnsErrorWithoutText(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// Emit one intermediate "thinking" line, then block. Without the cancel
	// guard, this partial text gets promoted to Result.Text and the run is
	// mistaken for a successful turn.
	backend := &scriptBackend{
		shellCmd:            `printf 'partial-thought\n'; sleep 60`,
		parseAsIntermediate: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New()
	done := make(chan RunResult, 1)
	go func() {
		done <- r.Run(ctx, backend, RunOptions{}, nil)
	}()

	// Give the script time to emit the intermediate line before cancelling.
	time.Sleep(250 * time.Millisecond)
	cancel()

	var res RunResult
	select {
	case res = <-done:
	case <-time.After(killGracePeriod + 5*time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if res.Error == nil {
		t.Fatalf("expected error after cancel, got success with Text=%q", res.Text)
	}
	if res.Text != "" {
		t.Fatalf("cancelled run must not expose partial intermediate as Text: %q", res.Text)
	}
}

// TestRunCancelDemotesPendingAsNarration asserts that when the run is
// cancelled mid-turn, whatever text had accumulated in `pending` is
// surfaced to the caller as a narration progress event — so the queue
// error branch can render the partial reply alongside the error marker.
// RunResult.Text must still stay empty so session state does not advance.
func TestRunCancelDemotesPendingAsNarration(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	backend := &scriptBackend{
		shellCmd:            `printf 'substantial narrative about to be cancelled\n'; sleep 60`,
		parseAsIntermediate: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &progressRecorder{}
	r := New()
	done := make(chan RunResult, 1)
	go func() {
		done <- r.Run(ctx, backend, RunOptions{}, rec.callback())
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()

	var res RunResult
	select {
	case res = <-done:
	case <-time.After(killGracePeriod + 5*time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if res.Error == nil {
		t.Fatalf("expected cancel error, got success")
	}
	if res.Text != "" {
		t.Fatalf("RunResult.Text must stay empty on cancel, got %q", res.Text)
	}
	narr := rec.narrationTexts()
	if len(narr) != 1 || narr[0] != "substantial narrative about to be cancelled" {
		t.Fatalf("pending must be demoted to narration on cancel, got %v", narr)
	}
}

// TestLookAheadEmitsAtParagraphBoundary asserts that a long assistant reply
// with a paragraph break in it leaks the first paragraph out as narration
// during the run, while the next paragraph stays in the buffer as the
// "more is coming" tail and is demoted to RunResult.Text at end of stream.
// The "\n\n" the runner cut on doubles as the natural log/final separator
// in queue.go's final delivery, so concat(log + "\n\n" + final) reproduces
// the original text exactly — no mid-sentence splits and no extra blank
// lines, which is the bug the paragraph-only cut policy exists to prevent.
func TestLookAheadEmitsAtParagraphBoundary(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// First paragraph is well above narrationFlushMinChars so the look-ahead
	// fires; second paragraph is above narrationFlushLookaheadChars so it
	// satisfies the "tail as proof" check.
	para1 := strings.Repeat("a", 250)
	para2 := strings.Repeat("b", 150)
	body := para1 + "\n\n" + para2
	// JSON strings cannot carry raw newlines — escape on the wire so the
	// claude parser hands back the original bytes once it decodes the line.
	jsonBody := strings.ReplaceAll(body, "\n", `\n`)
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonBody + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + jsonBody + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	narr := rec.narrationTexts()
	if len(narr) != 1 || narr[0] != para1 {
		t.Fatalf("expected one look-ahead narration with the first paragraph, got %v", narr)
	}
	if res.Text != para2 {
		t.Fatalf("final text must hold the second paragraph, got %q", res.Text)
	}
	// The "\n\n" between log narration and final text is supplied by
	// syncFinalMessageChain. Verifying here that body reassembles exactly
	// guards the contract the paragraph-only cut depends on.
	if narr[0]+"\n\n"+res.Text != body {
		t.Fatalf("narration + \"\\n\\n\" + final must reconstruct original body")
	}
}

func TestSuppressedNarrationKeepsLongReplyInFinalText(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	para1 := strings.Repeat("a", 250)
	para2 := strings.Repeat("b", 150)
	body := para1 + "\n\n" + para2
	jsonBody := strings.ReplaceAll(body, "\n", `\n`)
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonBody + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + jsonBody + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{SuppressNarrationProgress: true}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	if narr := rec.narrationTexts(); len(narr) != 0 {
		t.Fatalf("suppressed narration must not emit progress narration, got %v", narr)
	}
	if res.Text != body {
		t.Fatalf("final text must keep the full body when narration is suppressed")
	}
}

func TestSuppressedNarrationKeepsReplyAcrossRateLimitEvent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	body := "готовый ответ"
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + body + `"}]}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resets_at":2000000000,"rateLimitType":"five_hour","utilization":0.9}}`,
		`{"type":"result","session_id":"s1","result":"` + body + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{SuppressNarrationProgress: true}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	if narr := rec.narrationTexts(); len(narr) != 0 {
		t.Fatalf("suppressed narration must not emit progress narration, got %v", narr)
	}
	if res.Text != body {
		t.Fatalf("final text must survive a rate-limit progress event, got %q", res.Text)
	}
}

// TestLookAheadStaysSilentWithoutParagraphBreak asserts that a long but
// single-paragraph reply does NOT trigger look-ahead — there is nowhere
// safe to cut, so the whole thing flows through RunResult.Text. This is
// the price of the no-mid-sentence-split invariant: monolithic paragraphs
// remain monolithic rather than getting awkwardly chopped.
func TestLookAheadStaysSilentWithoutParagraphBreak(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	body := strings.Repeat("abcdefghij", 60) // 600 chars, no "\n\n"
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + body + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + body + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if got := rec.narrationTexts(); len(got) != 0 {
		t.Fatalf("monolithic paragraph must not emit narration, got %v", got)
	}
	if res.Text != body {
		t.Fatalf("final text must contain the full reply, got %q", res.Text)
	}
}

// TestLookAheadStaysSilentOnShortText asserts that a short assistant reply
// (below threshold) does NOT trigger any look-ahead emit — the entire text
// must arrive only as RunResult.Text, preserving today's "short answer has
// no `...` flicker" behavior (the pong-after-ping case).
func TestLookAheadStaysSilentOnShortText(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	short := "pong"
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + short + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + short + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if got := rec.narrationTexts(); len(got) != 0 {
		t.Fatalf("short reply must not produce any narration emit, got %v", got)
	}
	if res.Text != short {
		t.Fatalf("final text: got %q, want %q", res.Text, short)
	}
}

// TestIdleFlushCommitsCompletedParagraph asserts that when a paragraph has
// landed in the buffer and the backend then goes quiet past the idle
// timeout, the runner commits that completed paragraph as narration
// instead of waiting for the next event. Idle uses minBody=0, so even a
// short paragraph is eligible — the only requirement is a "\n\n" boundary
// and a tail large enough to count as evidence that work is still queued.
func TestIdleFlushCommitsCompletedParagraph(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	savedIdle := narrationIdleTimeout
	narrationIdleTimeout = 100 * time.Millisecond
	defer func() { narrationIdleTimeout = savedIdle }()

	para1 := "первый абзац"
	para2 := strings.Repeat("y", 150) // >= narrationFlushLookaheadChars
	body := para1 + "\n\n" + para2
	jsonBody := strings.ReplaceAll(body, "\n", `\n`)
	first := `{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`
	second := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonBody + `"}]}}`
	third := `{"type":"result","session_id":"s1","result":"` + jsonBody + `","is_error":false}`
	// Emit system + text, then sleep well past the (shortened) idle window
	// so the timer fires while the backend process is still alive. Then
	// the result line arrives and EOF closes the run.
	script := fmt.Sprintf("printf '%%s\\n%%s\\n' %s; sleep 0.4; printf '%%s\\n' %s",
		shQuote(first, second), shQuote(third))

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	narr := rec.narrationTexts()
	if len(narr) != 1 || narr[0] != para1 {
		t.Fatalf("idle must commit the completed first paragraph as narration, got %v", narr)
	}
	if res.Text != para2 {
		t.Fatalf("final text must hold the still-pending second paragraph, got %q", res.Text)
	}
}

// TestClaudePartialDeltaStreaming covers the --include-partial-messages
// flow end-to-end: text deltas arrive token-by-token, the runner raw-
// concatenates them, look-ahead fires on the paragraph boundary that
// forms inside the stream, and the trailing `assistant` event's text
// block is skipped (it would otherwise duplicate the whole reply because
// the parser remembers that partial-mode is active). Result: log gets
// the first paragraph, RunResult.Text gets the second — the visible chat
// reads as one continuous reply with no doubled content.
func TestClaudePartialDeltaStreaming(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	para1 := strings.Repeat("a", 250)
	para2 := strings.Repeat("b", 150)
	fullBody := para1 + "\n\n" + para2
	jsonFull := strings.ReplaceAll(fullBody, "\n", `\n`)

	// Three text-delta lines that together carry the full body. The split
	// straddles the paragraph break so the runner sees deltas land on
	// either side — the realistic shape of token streaming.
	d1 := para1[:200]
	d2 := para1[200:] + "\n\n" + para2[:50]
	d3 := para2[50:]
	deltaLine := func(text string) string {
		esc := strings.ReplaceAll(text, "\n", `\n`)
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + esc + `"}}}`
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		deltaLine(d1),
		deltaLine(d2),
		deltaLine(d3),
		// Final assistant event mirrors the streamed text — must be skipped.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonFull + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + jsonFull + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	narr := rec.narrationTexts()
	if len(narr) != 1 || narr[0] != para1 {
		t.Fatalf("look-ahead on streamed deltas must emit first paragraph once, got %v", narr)
	}
	if res.Text != para2 {
		t.Fatalf("final text must be the second paragraph (no duplicate from assistant event), got %q", res.Text)
	}
	if narr[0]+"\n\n"+res.Text != fullBody {
		t.Fatalf("narration + sep + final must reconstruct original body exactly")
	}
}

func TestClaudePartialDeltaSuppressedNarrationKeepsFullFinalText(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	para1 := strings.Repeat("a", 250)
	para2 := strings.Repeat("b", 150)
	fullBody := para1 + "\n\n" + para2
	jsonFull := strings.ReplaceAll(fullBody, "\n", `\n`)

	d1 := para1[:200]
	d2 := para1[200:] + "\n\n" + para2[:50]
	d3 := para2[50:]
	deltaLine := func(text string) string {
		esc := strings.ReplaceAll(text, "\n", `\n`)
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + esc + `"}}}`
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		deltaLine(d1),
		deltaLine(d2),
		deltaLine(d3),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonFull + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + jsonFull + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{SuppressNarrationProgress: true}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	if narr := rec.narrationTexts(); len(narr) != 0 {
		t.Fatalf("suppressed narration must not emit progress narration, got %v", narr)
	}
	if res.Text != fullBody {
		t.Fatalf("final text must keep the full streamed body when narration is suppressed")
	}
}

// TestPartialDeltaJoinsTextBlocksWithParagraphBreak mirrors the legacy
// TestClaudeStreamDemotesIntermediatesToNarration scenario under
// --include-partial-messages: two text-only assistant messages back to
// back, each streamed via deltas, must end up joined by "\n\n" exactly as
// the legacy non-delta path does. Without the content_block_stop →
// EventTextBoundary → markBlockBoundary plumbing, the deltas from block 2
// would raw-concat onto block 1 and the visible chat would read
// "block1block2" with no paragraph between them.
func TestPartialDeltaJoinsTextBlocksWithParagraphBreak(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	block1 := strings.Repeat("a", 50)
	block2 := strings.Repeat("b", 50)
	deltaLine := func(text string) string {
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}}`
	}
	stopLine := `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`
	startLine := `{"type":"stream_event","event":{"type":"message_start"}}`

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		startLine,
		deltaLine(block1),
		stopLine,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + block1 + `"}]}}`,
		startLine,
		deltaLine(block2),
		stopLine,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + block2 + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + block1 + `\n\n` + block2 + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	// Both blocks are below look-ahead threshold, so neither leaks via
	// narration — the whole reply ends up in RunResult.Text, joined by
	// the inserted paragraph break.
	if got := rec.narrationTexts(); len(got) != 0 {
		t.Fatalf("blocks below threshold must stay in buffer, got narration %v", got)
	}
	want := block1 + "\n\n" + block2
	if res.Text != want {
		t.Fatalf("two delta blocks must join with \"\\n\\n\": got %q, want %q", res.Text, want)
	}
}

// TestLookAheadSkipsCutInsideCodeFence asserts the cut policy never lands
// at a "\n\n" that sits inside a fenced code block. A reply that wraps a
// long code block can legitimately contain a blank line between hunks of
// code; cutting there would emit narration with an opening "```" but no
// closer, leaving the final answer body with a closer but no opener — the
// rendered chat would show stray backticks instead of one code block. The
// fixture below is shaped so the only "\n\n" inside the body lies between
// the fence markers, and a real outside-fence boundary exists right after
// the closing fence — look-ahead must pick the second one.
func TestLookAheadSkipsCutInsideCodeFence(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// Prelude makes the inside-fence "\n\n" sit past narrationFlushMinChars
	// so the bug would actually trip if cut policy were fence-blind.
	prelude := strings.Repeat("x", 250)
	codeBlock := "```go\nline one\n\nline two\n```"
	tail := strings.Repeat("y", 150)
	body := prelude + "\n" + codeBlock + "\n\n" + tail
	jsonBody := strings.ReplaceAll(body, "\n", `\n`)
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + jsonBody + `"}]}}`,
		`{"type":"result","session_id":"s1","result":"` + jsonBody + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}

	narr := rec.narrationTexts()
	if len(narr) != 1 {
		t.Fatalf("expected one narration (after the code fence), got %d: %v", len(narr), narr)
	}
	wantBody := prelude + "\n" + codeBlock
	if narr[0] != wantBody {
		t.Fatalf("narration must end at the post-fence \"\\n\\n\", got %q…%q",
			narr[0][:40], narr[0][len(narr[0])-40:])
	}
	if res.Text != tail {
		t.Fatalf("final text must be the post-fence tail, got %q", res.Text)
	}
	if narr[0]+"\n\n"+res.Text != body {
		t.Fatalf("narration + sep + final must reconstruct original body exactly")
	}
}

// TestIdleStaysSilentWithoutParagraphBreak asserts the conservative side
// of the idle policy: a buffer that contains text but no "\n\n" never
// fires idle, because doing so would split a sentence at a place the
// final delivery cannot stitch back without inserting a blank line.
func TestIdleStaysSilentWithoutParagraphBreak(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	savedIdle := narrationIdleTimeout
	narrationIdleTimeout = 100 * time.Millisecond
	defer func() { narrationIdleTimeout = savedIdle }()

	body := "длинная цельная фраза без переноса абзаца которая просто висит"
	first := `{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`
	second := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + body + `"}]}}`
	third := `{"type":"result","session_id":"s1","result":"` + body + `","is_error":false}`
	script := fmt.Sprintf("printf '%%s\\n%%s\\n' %s; sleep 0.4; printf '%%s\\n' %s",
		shQuote(first, second), shQuote(third))

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if got := rec.narrationTexts(); len(got) != 0 {
		t.Fatalf("idle must not emit a half-sentence, got %v", got)
	}
	if res.Text != body {
		t.Fatalf("final text: got %q, want %q", res.Text, body)
	}
}

// errResultBackend emits an errored EventResult on the "ERR" line and treats
// every other line as assistant narration.
type errResultBackend struct{ script string }

func (b *errResultBackend) Name() string { return "errbk" }

func (b *errResultBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *errResultBackend) ParseEvent(line []byte) ([]Event, bool) {
	if string(line) == "ERR" {
		return []Event{{Type: EventResult, Error: "boom"}}, true
	}
	return []Event{{Type: EventIntermediate, Text: string(line)}}, true
}

// TestRunErroredResultReachesWaitAndReturns guards the unified cleanup path: an
// errored result mid-stream must NOT short-circuit out of Run. The loop keeps
// draining the backend's remaining output, reaps the process via cmd.Wait, and
// returns the error — it must never hang or orphan a still-writing backend.
func TestRunErroredResultReachesWaitAndReturns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	// Error arrives, then the backend emits MORE output before exiting. The old
	// early-return would have stopped at ERR and never consumed the trailing
	// line; the unified path keeps draining, so the trailing line must surface
	// as narration. That post-error consumption is what distinguishes this from
	// the buggy behavior — a plain "returns an error" assertion would pass on
	// both the old and new code.
	script := "printf 'hello\\nERR\\ntrailing-after-error\\n'"

	rec := &progressRecorder{}
	r := New()
	done := make(chan RunResult, 1)
	go func() {
		done <- r.Run(context.Background(), &errResultBackend{script: script}, RunOptions{}, rec.callback())
	}()

	select {
	case res := <-done:
		if res.Error == nil {
			t.Fatal("expected error from errored result, got nil")
		}
		if !strings.Contains(res.Error.Error(), "boom") {
			t.Fatalf("error = %v; want it to carry the backend message", res.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung after an errored result event — cleanup path regressed")
	}

	var sawTrailing bool
	for _, n := range rec.narrationTexts() {
		if strings.Contains(n, "trailing-after-error") {
			sawTrailing = true
		}
	}
	if !sawTrailing {
		t.Fatalf("output after the error event was not drained; narration = %v", rec.narrationTexts())
	}
}

func TestSuppressedNarrationKeepsReplyAcrossCompactEvent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	body := "готовый ответ"
	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + body + `"}]}}`,
		`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":180000,"postTokens":9000}}`,
		`{"type":"result","session_id":"s1","result":"` + body + `","is_error":false}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	rec := &progressRecorder{}
	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{SuppressNarrationProgress: true}, rec.callback())
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if narr := rec.narrationTexts(); len(narr) != 0 {
		t.Fatalf("suppressed narration must not emit progress narration, got %v", narr)
	}
	if res.Text != body {
		t.Fatalf("final text must survive a compact progress event, got %q", res.Text)
	}
}
