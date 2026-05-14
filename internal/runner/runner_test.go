package runner

import (
	"context"
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

	cmd := `cd "` + home + `/very/long/path/with/many/segments/for/testing/that/keeps/going/and/going" && echo done`
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
		return []Event{{Type: "intermediate", Text: string(line)}}, true
	}
	return nil, false
}

// collectProgress builds a ProgressFunc that records every event so tests
// can assert the live narration/tool log.
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

// TestClaudeStreamEmitsTextBlocksAsNarrationLive encodes the delivery
// contract for Claude streams with multiple text blocks around tool calls.
// Each assistant text block surfaces as a narration progress event the
// instant it is parsed — no buffering, no waiting for a tool boundary or
// end-of-stream. The frontend renders the live progress log directly, so the
// runner does not need to pick a "final answer" string at end-of-run.
func TestClaudeStreamEmitsTextBlocksAsNarrationLive(t *testing.T) {
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
		// Second text block without an intervening tool — under live
		// streaming each becomes its own narration event, so the
		// frontend can render the first immediately rather than waiting
		// for the pair to complete.
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
	wantOrder := [][2]string{
		{"narration", "before tool"},
		{"tool", "📖 Read: /tmp/x"},
		{"narration", "проверка 2\n\nKLODIN"},
		{"narration", "after tool"},
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
	want := [][2]string{
		{"narration", "A"},
		{"tool", "📖 Read: /tmp/r"},
		{"narration", "B"},
		{"tool", "📝 Write: /tmp/w"},
		{"narration", "C"},
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
// in the log, including the final text before EOF.
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
	// Every turn's assistant text surfaces as narration the moment it is
	// parsed — including the trailing one. The frontend renders these
	// directly; there is no separate "final answer" string at end-of-run.
	want := [][2]string{
		{"narration", "Жду поллер"},
		{"tool", "📖 Read: /tmp/poll.out"},
		{"narration", "SSH вернулся"},
		{"tool", "⚙️ Bash: `uname -a`"},
		{"narration", "Готово"},
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
// this test covers both ParseEvent and the runner loop end-to-end.
type stdinEchoBackend struct {
	script string
}

func (b *stdinEchoBackend) Name() string { return "claude" }

func (b *stdinEchoBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *stdinEchoBackend) ParseEvent(line []byte) ([]Event, bool) {
	return (&ClaudeBackend{}).ParseEvent(line)
}

// TestCodexStreamEmitsAgentMessagesAsNarrationLive mirrors the Claude
// narration + ordering tests for codex: agent_message entries become
// narration immediately, preserving order around command_execution events.
func TestCodexStreamEmitsAgentMessagesAsNarrationLive(t *testing.T) {
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
	want := [][2]string{
		{"narration", "начинаю"},
		{"tool", "⚙️ Bash: `ls /tmp`"},
		{"narration", "готово"},
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

	// Emit one intermediate "thinking" line, then block. Cancellation must
	// still return an error even though partial text has already streamed.
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
		t.Fatal("expected error after cancel, got success")
	}
}

// TestRunCancelKeepsStreamedNarrationVisible asserts that when the run is
// cancelled mid-turn, the assistant text already streamed out as narration
// stays in the recorder — so the queue's error branch can render the partial
// reply alongside the error marker. RunResult itself only carries Error;
// the partial body is in onProgress history, not on the result.
func TestRunCancelKeepsStreamedNarrationVisible(t *testing.T) {
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
	narr := rec.narrationTexts()
	if len(narr) != 1 || narr[0] != "substantial narrative about to be cancelled" {
		t.Fatalf("narration must already be streamed before cancel, got %v", narr)
	}
}
