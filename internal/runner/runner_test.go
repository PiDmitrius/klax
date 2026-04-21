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

// scriptBackend runs an arbitrary shell command. Used to simulate the
// codex npm-shim → rust-grandchild topology in process-lifecycle tests
// without depending on a real backend binary. parseAsIntermediate treats
// every stdout line as a codex-style "intermediate" thinking update so we
// can test that cancellation does not let a partial thought leak out as
// a successful answer.
type scriptBackend struct {
	shellCmd             string
	parseAsIntermediate  bool
}

func (b *scriptBackend) Name() string { return "script" }

func (b *scriptBackend) BuildCmd(_ RunOptions) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", b.shellCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (b *scriptBackend) ParseEvent(line []byte) (Event, bool) {
	if b.parseAsIntermediate {
		return Event{Type: "intermediate", Text: string(line)}, true
	}
	return Event{}, false
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
