package main

import (
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/runner"
)

func TestParseTTYArgsWrapsClaudeInvocation(t *testing.T) {
	opts, err := parseTTYArgs([]string{
		"claude",
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--disallowed-tools", "Agent,AskUserQuestion",
		"--permission-mode", "bypassPermissions",
		"--model", "claude-fable-5",
		"--effort", "high",
		"--append-system-prompt", "stay terse",
		"--resume", "abc-123",
		"--timeout", "3s",
		"hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ClaudePath != "claude" ||
		opts.Prompt != "hello" ||
		opts.DisallowedTools != "Agent,AskUserQuestion" ||
		opts.PermissionMode != "bypassPermissions" ||
		opts.Model != "claude-fable-5" ||
		opts.Effort != "high" ||
		opts.AppendSystemPrompt != "stay terse" ||
		opts.Resume != "abc-123" ||
		opts.Timeout != 3*time.Second {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestParseTTYArgsAcceptsDoubleDash(t *testing.T) {
	opts, err := parseTTYArgs([]string{"--", "claude", "-p", "--output-format", "stream-json", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ClaudePath != "claude" || opts.Prompt != "hello" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestParseTTYArgsRejectsUnknownFlags(t *testing.T) {
	if _, err := parseTTYArgs([]string{"claude", "--danger", "hello"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

// TestParseTTYArgsAcceptsRealBuildCmdArgv couples the tty parser to the actual
// argv ClaudeBackend.BuildCmd emits for a TTY run: every flag BuildCmd can add
// must be one parseTTYArgs whitelists. A future flag added to BuildCmd that the
// parser doesn't handle trips here instead of silently breaking tty in prod.
func TestParseTTYArgsAcceptsRealBuildCmdArgv(t *testing.T) {
	// Couple the tty parser to the real claude flag set WITHOUT needing the
	// claude binary installed (BuildCmd resolves the binary and would otherwise
	// have to be skipped in a bare CI). BuildClaudeArgs is exactly what BuildCmd
	// wraps as ["tty", claudebin, <these args>]; parseTTYArgs consumes
	// everything after the "tty" token, i.e. [claudebin, <these args>]. A future
	// flag added to BuildClaudeArgs that the parser doesn't handle trips here.
	claudeArgs := runner.BuildClaudeArgs(runner.RunOptions{
		Model:              "claude-fable-5",
		Effort:             "high",
		AppendSystemPrompt: "stay terse",
		SessionID:          "abc-123",
		Sandbox:            "off",
	})
	argv := append([]string{"claude"}, claudeArgs...)
	opts, err := parseTTYArgs(argv)
	if err != nil {
		t.Fatalf("parseTTYArgs rejected real BuildClaudeArgs argv %v: %v", argv, err)
	}
	if opts.Model != "claude-fable-5" || opts.Effort != "high" ||
		opts.AppendSystemPrompt != "stay terse" || opts.Resume != "abc-123" ||
		opts.PermissionMode != "bypassPermissions" {
		t.Fatalf("opts not populated from BuildClaudeArgs argv: %+v", opts)
	}
	// In -p mode the prompt travels via stdin, never argv — so the tty parser
	// must not have picked one up positionally.
	if opts.Prompt != "" {
		t.Fatalf("prompt should arrive via stdin, got %q", opts.Prompt)
	}
}
