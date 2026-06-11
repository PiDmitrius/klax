package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeClaude is a stand-in for the interactive claude TUI: it fires the
// SessionStart hook, reads the typed prompt off its tty, appends this turn's
// lines to the transcript, and fires Stop. The hook relay script lives next
// to the FIFO, so it is derived from CLAUDETTY_FIFO.
//
// %[1]s = transcript path, %[2]s = session id, %[3]s = extra shell before
// reading the prompt (e.g. swallowing the first Enter).
const fakeClaude = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
%[3]s
# The pty is in canonical mode (the fake never sets raw), so the driver's
# trailing \r arrives as \n — a plain line read picks up the typed prompt.
IFS= read -r prompt
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$prompt" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" Stop
sleep 60
`

func writeFake(t *testing.T, transcriptPath, sessionID, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaude, transcriptPath, sessionID, extra)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type wireEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func parseWire(t *testing.T, out []byte) []wireEvent {
	t.Helper()
	var evs []wireEvent
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev wireEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("non-JSON output line %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func assistantTexts(evs []wireEvent) []string {
	var texts []string
	for _, ev := range evs {
		if ev.Type != "assistant" || ev.Message == nil {
			continue
		}
		for _, b := range ev.Message.Content {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
	}
	return texts
}

func TestRunNewSession(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	fake := writeFake(t, tp, "sess-new", "")

	var out bytes.Buffer
	code, err := Run(context.Background(), &out, Options{
		Prompt:     "ping",
		ClaudePath: fake,
		Timeout:    30 * time.Second,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}

	evs := parseWire(t, out.Bytes())
	if evs[0].Type != "system" || evs[0].SessionID != "sess-new" {
		t.Fatalf("first event = %+v, want system init with session id", evs[0])
	}
	if got := assistantTexts(evs); len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("assistant texts = %q, want [PONG]", got)
	}
	last := evs[len(evs)-1]
	if last.Type != "result" || last.Result != "PONG" || last.IsError {
		t.Fatalf("result = %+v", last)
	}
}

// TestRunResumeSkipsHistory is the Telegram-flood regression: interactive
// `--resume` appends to the same transcript file, so its pre-existing
// content must never be re-emitted as fresh events.
func TestRunResumeSkipsHistory(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	history := `{"type":"user","sessionId":"sess-resumed","message":{"role":"user","content":"old question"}}
{"type":"assistant","sessionId":"sess-resumed","message":{"model":"fake-model","content":[{"type":"text","text":"HISTORY-1"}],"usage":{"input_tokens":5,"output_tokens":5}}}
{"type":"assistant","sessionId":"sess-resumed","message":{"model":"fake-model","content":[{"type":"text","text":"HISTORY-2"}],"usage":{"input_tokens":5,"output_tokens":5}}}
`
	if err := os.WriteFile(tp, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := writeFake(t, tp, "sess-resumed", "")

	var out bytes.Buffer
	code, err := Run(context.Background(), &out, Options{
		Prompt:     "ping",
		Resume:     "sess-resumed",
		ClaudePath: fake,
		Timeout:    30 * time.Second,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}

	if strings.Contains(out.String(), "HISTORY") {
		t.Fatalf("resume history leaked into output:\n%s", out.String())
	}
	evs := parseWire(t, out.Bytes())
	if got := assistantTexts(evs); len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("assistant texts = %q, want [PONG]", got)
	}
	last := evs[len(evs)-1]
	if last.Result != "PONG" {
		t.Fatalf("result = %+v, want PONG (not polluted by history)", last)
	}
}

// TestRunRetypesSwallowedEnter: the first Enter lands while the fake is not
// reading (Ink mid-render); the driver must detect the silent transcript and
// resend it.
func TestRunRetypesSwallowedEnter(t *testing.T) {
	if testing.Short() {
		t.Skip("retry path waits ~5s")
	}
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	// Swallow the prompt and the first Enter, then wait for the retry \r.
	extra := `IFS= read -r swallowed`
	fake := writeFake(t, tp, "sess-retry", extra)

	var out bytes.Buffer
	code, err := Run(context.Background(), &out, Options{
		Prompt:     "ping",
		ClaudePath: fake,
		Timeout:    30 * time.Second,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}
	evs := parseWire(t, out.Bytes())
	last := evs[len(evs)-1]
	if last.Type != "result" || last.IsError {
		t.Fatalf("result = %+v", last)
	}
}

// fakeClaudeLateLeak writes a stray assistant line AFTER the prompt is typed
// (the read unblocks only once the driver types) but BEFORE the user echo.
// On a RESUME such a line is unverified prior-session history and must be
// discarded — the regression is "history can leak after promptSent but before
// echo". (On a fresh session there is no history to confuse it with, so the
// discard is intentionally keyed on echo only for resumes.)
const fakeClaudeLateLeak = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"LEAK"}],"usage":{"input_tokens":9,"output_tokens":9}}}\n' "$sid" >> "$tp"
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$prompt" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" Stop
sleep 60
`

func TestRunDiscardsLeakBeforeEcho(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeLateLeak, tp, "sess-leak")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// Resume set: the pre-echo discard is load-bearing only when the file may
	// hold a prior session's history.
	code, err := Run(context.Background(), &out, Options{Prompt: "ping", Resume: "sess-leak", ClaudePath: path, Timeout: 30 * time.Second})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}
	if strings.Contains(out.String(), "LEAK") {
		t.Fatalf("pre-echo line leaked into output:\n%s", out.String())
	}
	if got := assistantTexts(parseWire(t, out.Bytes())); len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("assistant texts = %q, want [PONG]", got)
	}
}

// fakeClaudeAPIError emits an isApiErrorMessage line (with only an `error`
// field) after the echo and never fires Stop — the driver must surface the
// error text as the result and end the turn instead of waiting forever.
const fakeClaudeAPIError = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$prompt" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","isApiErrorMessage":true,"error":"API Error: Overloaded","message":{"model":"fake-model","content":[{"type":"text","text":""}]}}\n' "$sid" >> "$tp"
sleep 60
`

func TestRunSurfacesAPIError(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeAPIError, tp, "sess-apierr")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, _ := Run(context.Background(), &out, Options{Prompt: "ping", ClaudePath: path, Timeout: 30 * time.Second})
	if code != 1 {
		t.Fatalf("Run code = %d, want 1\noutput:\n%s", code, out.String())
	}
	last := parseWire(t, out.Bytes())
	res := last[len(last)-1]
	if res.Type != "result" || !res.IsError {
		t.Fatalf("result = %+v, want error result", res)
	}
	if !strings.Contains(res.Result, "Overloaded") {
		t.Fatalf("result text = %q, want the API error surfaced", res.Result)
	}
}

// fakeClaudeEchoMismatch echoes a user line whose content does NOT contain the
// prompt needle (simulating a needle false-negative, e.g. an escaping
// divergence). On a fresh session there is no history to skip, so the answer
// must still emit — echo detection must not be load-bearing for output.
const fakeClaudeEchoMismatch = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"ZZZ-does-not-match"}}\n' "$sid" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" Stop
sleep 60
`

func TestRunFreshSessionEmitsDespiteEchoMismatch(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeEchoMismatch, tp, "sess-fresh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// No Resume: fresh session. Even though the echo never matches the needle,
	// the turn must still produce its answer rather than a silent empty result.
	code, err := Run(context.Background(), &out, Options{Prompt: "ping", ClaudePath: path, Timeout: 30 * time.Second})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}
	evs := parseWire(t, out.Bytes())
	if got := assistantTexts(evs); len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("assistant texts = %q, want [PONG] (fresh turn must emit despite echo miss)", got)
	}
	last := evs[len(evs)-1]
	if last.Type != "result" || last.Result != "PONG" || last.IsError {
		t.Fatalf("result = %+v, want PONG success", last)
	}
}

// fakeClaudeAPIErrorPreEcho fires an API error before any prompt echo on a
// resumed session. The error is the turn's outcome and must be surfaced, not
// discarded along with the resumed history.
const fakeClaudeAPIErrorPreEcho = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"assistant","sessionId":"%%s","isApiErrorMessage":true,"error":"API Error: Overloaded","message":{"model":"fake-model","content":[{"type":"text","text":""}]}}\n' "$sid" >> "$tp"
sleep 60
`

func TestRunSurfacesAPIErrorBeforeEchoOnResume(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeAPIErrorPreEcho, tp, "sess-apierr2")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, _ := Run(context.Background(), &out, Options{Prompt: "ping", Resume: "sess-apierr2", ClaudePath: path, Timeout: 30 * time.Second})
	if code != 1 {
		t.Fatalf("Run code = %d, want 1\noutput:\n%s", code, out.String())
	}
	last := parseWire(t, out.Bytes())
	res := last[len(last)-1]
	if res.Type != "result" || !res.IsError || !strings.Contains(res.Result, "Overloaded") {
		t.Fatalf("result = %+v, want surfaced API error", res)
	}
}

// fakeClaudeFreshShrinkReplay drives a fresh session whose echo does NOT match
// the needle, emits one assistant line, then rewrites the transcript shorter
// (an in-place compaction). Submission must be confirmed — and the tailer
// frozen — on the fresh session's first user line despite the needle miss, so
// the shrink does not reset the read offset and replay the already-emitted
// line.
const fakeClaudeFreshShrinkReplay = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"ZZZ-no-match"}}\n' "$sid" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"ONCE"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
sleep 0.4
: > "$tp"
printf '{"type":"summary","summary":"compacted"}\n' >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"ONCE"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
sleep 0.4
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" Stop
sleep 60
`

func TestRunFreshConfirmsSubmissionAndFreezes(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeFreshShrinkReplay, tp, "sess-freeze")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := Run(context.Background(), &out, Options{Prompt: "ping", ClaudePath: path, Timeout: 30 * time.Second})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}
	// Frozen after the fresh session confirmed submission, so the post-echo
	// shrink does not replay the assistant line.
	if n := strings.Count(out.String(), `"text":"ONCE"`); n != 1 {
		t.Fatalf("assistant line emitted %d times, want 1 (no shrink replay):\n%s", n, out.String())
	}
}

// fakeClaudeAbort accepts the prompt, records its own pid, then runs forever
// without firing Stop — a turn that is still in flight when /abort lands.
// %[3]s = path it writes its pid to so the test can prove the group was reaped.
const fakeClaudeAbort = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
echo $$ > "%[3]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$prompt" >> "$tp"
# A long turn that never fires Stop; the test aborts it mid-flight.
sleep 120
`

func processAlive(pid int) bool {
	// kill(pid, 0) probes existence without signalling: nil = alive (incl.
	// not-yet-reaped zombie), ESRCH = gone.
	return syscall.Kill(pid, 0) == nil
}

func waitForPidFile(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake claude never recorded its pid")
	return 0
}

// TestRunAbortReapsClaude is the /abort teardown regression. claude runs in its
// own Setsid session, so the wrapper's process-group SIGTERM cannot reach it —
// the kill only happens through driver.Run's defers. Cancelling ctx must (1)
// end the turn promptly rather than wait out the Timeout, and (2) reap claude's
// group, or an aborted turn orphans claude and it keeps burning quota.
func TestRunAbortReapsClaude(t *testing.T) {
	if testing.Short() {
		t.Skip("abort path spawns a real process and waits on teardown")
	}
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	pidFile := filepath.Join(t.TempDir(), "claude.pid")
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeAbort, tp, "sess-abort", pidFile)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		out  bytes.Buffer
		code int
		rerr error
	)
	done := make(chan struct{})
	go func() {
		// Timeout is high on purpose: without the ctx check, Run would block
		// here ~30s and the 5s deadline below would catch the regression.
		code, rerr = Run(ctx, &out, Options{Prompt: "ping", ClaudePath: path, Timeout: 30 * time.Second})
		close(done)
	}()

	pid := waitForPidFile(t, pidFile)
	time.Sleep(500 * time.Millisecond) // let the turn get in flight
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of abort — ctx cancel ignored")
	}
	if rerr != nil {
		t.Fatalf("Run err = %v", rerr)
	}
	if code != 1 {
		t.Fatalf("abort exit code = %d, want 1\noutput:\n%s", code, out.String())
	}
	evs := parseWire(t, out.Bytes())
	last := evs[len(evs)-1]
	if last.Type != "result" || !last.IsError || last.Result != "turn aborted" {
		t.Fatalf("terminal event = %+v, want is_error result \"turn aborted\"", last)
	}
	// The crux: claude's separate Setsid group was reaped by the deferred
	// teardown. The Wait goroutine releases the pid a beat after Run returns.
	for i := 0; i < 100 && processAlive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("claude (pid %d) still alive after abort — teardown defer did not reap it", pid)
	}
}

func TestContextWindowForModel(t *testing.T) {
	cases := map[string]int{
		"fable[1m]":      1_000_000,
		"sonnet[1m]":     1_000_000,
		"fable":          1_000_000,
		"claude-fable-5": 1_000_000,
		"opus":           200_000,
		"sonnet":         200_000,
		"":               200_000,
	}
	for model, want := range cases {
		if got := contextWindowForModel(model); got != want {
			t.Errorf("contextWindowForModel(%q) = %d, want %d", model, got, want)
		}
	}
}

// fakeClaudeCompact models the TUI's pre-flight auto-compact: after the
// prompt is typed (read off the tty) but BEFORE its echo reaches the
// transcript, a fresh compact_boundary is appended — exactly the window in
// which the real TUI compacts a near-full resumed session.
const fakeClaudeCompact = `#!/bin/bash
dir=$(dirname "$CLAUDETTY_FIFO")
hook="$dir/hook.sh"
tp="%[1]s"
sid="%[2]s"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" SessionStart
IFS= read -r prompt
now=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%S.000Z)
printf '{"type":"system","subtype":"compact_boundary","sessionId":"%%s","timestamp":"%%s","compactMetadata":{"trigger":"auto","preTokens":212091,"postTokens":7841}}\n' "$sid" "$now" >> "$tp"
sleep 0.4
printf '{"type":"user","sessionId":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$prompt" >> "$tp"
printf '{"type":"assistant","sessionId":"%%s","message":{"model":"fake-model","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":2,"output_tokens":1}}}\n' "$sid" >> "$tp"
printf '{"session_id":"%%s","transcript_path":"%%s"}' "$sid" "$tp" | "$hook" Stop
sleep 60
`

// TestRunResumeForwardsFreshPreEchoCompact: a boundary written during this
// turn (pre-flight auto-compact) must reach the wire even though it lands
// before the prompt echo — while a boundary replayed out of resumed history
// must stay absorbed.
func TestRunResumeForwardsFreshPreEchoCompact(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	history := `{"type":"user","sessionId":"sess-cb","message":{"role":"user","content":"old question"}}
{"type":"system","subtype":"compact_boundary","sessionId":"sess-cb","timestamp":"2026-01-01T00:00:00.000Z","compactMetadata":{"trigger":"manual","preTokens":31337,"postTokens":900}}
{"type":"assistant","sessionId":"sess-cb","message":{"model":"fake-model","content":[{"type":"text","text":"HISTORY-1"}],"usage":{"input_tokens":5,"output_tokens":5}}}
`
	if err := os.WriteFile(tp, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	body := fmt.Sprintf(fakeClaudeCompact, tp, "sess-cb")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := Run(context.Background(), &out, Options{
		Prompt:     "ping",
		Resume:     "sess-cb",
		ClaudePath: path,
		Timeout:    30 * time.Second,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\noutput:\n%s", code, err, out.String())
	}

	s := out.String()
	if strings.Contains(s, "31337") || strings.Contains(s, "HISTORY") {
		t.Fatalf("resumed history (old boundary) leaked into output:\n%s", s)
	}
	if n := strings.Count(s, `"compact_boundary"`); n != 1 {
		t.Fatalf("compact_boundary lines = %d, want exactly the fresh one:\n%s", n, s)
	}
	if !strings.Contains(s, "212091") {
		t.Fatalf("fresh pre-echo boundary was absorbed:\n%s", s)
	}
	if strings.Index(s, "212091") > strings.Index(s, "PONG") {
		t.Fatalf("boundary must be emitted before the turn's content:\n%s", s)
	}
	evs := parseWire(t, out.Bytes())
	if evs[0].Type != "system" || evs[0].SessionID != "sess-cb" {
		t.Fatalf("first event = %+v, want system init", evs[0])
	}
	if got := assistantTexts(evs); len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("assistant texts = %q, want [PONG]", got)
	}
}
