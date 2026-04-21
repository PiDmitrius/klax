package runner

import (
	"os/exec"
	"strings"
	"testing"
)

func assertSetpgid(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("expected Setpgid to be set so /abort can signal grandchildren")
	}
}

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

func TestClaudeBuildsWithOwnProcessGroup(t *testing.T) {
	cmd, err := (&ClaudeBackend{}).BuildCmd(RunOptions{Sandbox: "off"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	assertSetpgid(t, cmd)
}

func TestCodexBuildsWithOwnProcessGroup(t *testing.T) {
	cmd, err := (&CodexBackend{}).BuildCmd(RunOptions{Sandbox: "off"})
	if err != nil {
		t.Fatalf("BuildCmd: %v", err)
	}
	assertSetpgid(t, cmd)
}

func TestCodexParsesMcpToolCallAsTool(t *testing.T) {
	b := &CodexBackend{}
	line := []byte(`{"type":"item.started","item":{"id":"item_0","type":"mcp_tool_call","server":"codex_apps","tool":"github_get_profile","arguments":{},"status":"in_progress"}}`)

	events, ok := b.ParseEvent(line)
	if !ok {
		t.Fatalf("ParseEvent returned ok=false")
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "tool" {
		t.Fatalf("expected tool event, got %q (text=%q)", ev.Type, ev.Text)
	}
	if ev.Tool.Name != "MCP" {
		t.Fatalf("expected Tool.Name=MCP, got %q", ev.Tool.Name)
	}
	got := ev.Tool.String()
	if !strings.Contains(got, "codex_apps.github_get_profile") {
		t.Fatalf("expected server.tool in preview, got %q", got)
	}
}
