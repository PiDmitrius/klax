package runner

import (
	"strings"
	"testing"
)

func TestClaudeSandboxOffSetsBypassPermissions(t *testing.T) {
	cmd, err := (&ClaudeBackend{}).BuildCmd(RunOptions{Sandbox: "off"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--permission-mode bypassPermissions") {
		t.Fatalf("expected bypass permissions, got %q", args)
	}
}

func TestClaudeSandboxOnOmitsPermissionMode(t *testing.T) {
	cmd, err := (&ClaudeBackend{}).BuildCmd(RunOptions{Sandbox: "on"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "--permission-mode") {
		t.Fatalf("expected no explicit permission mode, got %q", args)
	}
}

func TestCodexSandboxOffSetsDangerFlags(t *testing.T) {
	cmd, err := (&CodexBackend{}).BuildCmd(RunOptions{Sandbox: "off"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--sandbox danger-full-access") {
		t.Fatalf("expected danger sandbox on new exec, got %q", args)
	}
}

func TestCodexSandboxOffResumeSetsDangerBypass(t *testing.T) {
	cmd, err := (&CodexBackend{}).BuildCmd(RunOptions{Sandbox: "off", SessionID: "thread"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected danger bypass on resume, got %q", args)
	}
}

func TestCodexSandboxOnOmitsSandboxFlags(t *testing.T) {
	cmd, err := (&CodexBackend{}).BuildCmd(RunOptions{Sandbox: "on"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "--sandbox") || strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || strings.Contains(args, "--full-auto") {
		t.Fatalf("expected no sandbox flags in safe mode, got %q", args)
	}
}
