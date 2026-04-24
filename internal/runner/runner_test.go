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

// TestClaudeStreamLocksFinalAfterResult asserts that text events arriving
// AFTER `result` do not overwrite the locked-in final answer. Backends
// shouldn't emit such lines, but a stray one must not silently clobber
// what the user sees.
func TestClaudeStreamLocksFinalAfterResult(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	lines := []string{
		`{"type":"system","session_id":"s1","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"real answer"}]}}`,
		`{"type":"result","session_id":"s1","result":"real answer","is_error":false}`,
		// Stray text after result — must be ignored.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"LATE GARBAGE"}]}}`,
	}
	script := "printf '%s\\n' " + shQuote(lines...)

	r := New()
	res := r.Run(context.Background(), &stdinEchoBackend{script: script}, RunOptions{}, nil)
	if res.Error != nil {
		t.Fatalf("Run error: %v", res.Error)
	}
	if res.Text != "real answer" {
		t.Fatalf("post-result events must not overwrite final, got %q", res.Text)
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
