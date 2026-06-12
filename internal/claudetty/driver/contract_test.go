package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureClaudeContractCreatesMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	changed, err := ensureClaudeContract(p, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating the file must report a change")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["resumeReturnDismissed"] != true || cfg["hasCompletedOnboarding"] != true {
		t.Fatalf("global flags not set: %v", cfg)
	}
	proj, _ := cfg["projects"].(map[string]any)["/home/u"].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true {
		t.Fatalf("cwd not trusted: %v", cfg)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("new config must be 0600, got %v", fi.Mode().Perm())
	}
}

func TestEnsureClaudeContractMergesPreservingContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	// 9007199254740993 exceeds 2^53: it survives only if numbers round-trip
	// as json.Number, not float64.
	src := `{
  "numStartups": 9007199254740993,
  "resumeReturnDismissed": false,
  "projects": {
    "/home/u": {"allowedTools": ["Bash"], "hasTrustDialogAccepted": false},
    "/other": {"hasTrustDialogAccepted": true}
  }
}`
	if err := os.WriteFile(p, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureClaudeContract(p, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("false flags must be rewritten to true")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer mangled: %s", data)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["resumeReturnDismissed"] != true || cfg["hasCompletedOnboarding"] != true {
		t.Fatalf("global flags not set: %v", cfg)
	}
	projects := cfg["projects"].(map[string]any)
	proj := projects["/home/u"].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true {
		t.Fatalf("cwd not trusted: %v", proj)
	}
	if tools, _ := proj["allowedTools"].([]any); len(tools) != 1 || tools[0] != "Bash" {
		t.Fatalf("sibling project keys lost: %v", proj)
	}
	if _, ok := projects["/other"]; !ok {
		t.Fatalf("other project entries lost: %v", projects)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Fatalf("existing perms must be preserved, got %v", fi.Mode().Perm())
	}
}

func TestEnsureClaudeContractIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	// Deliberately non-canonical formatting: an idempotent call must leave
	// the bytes untouched, proving it didn't rewrite the file.
	src := "{\"resumeReturnDismissed\": true,\t\"hasCompletedOnboarding\": true," +
		"\"projects\":{\"/home/u\":{\"hasTrustDialogAccepted\":true}}}"
	if err := os.WriteFile(p, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureClaudeContract(p, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("fully satisfied contract must not report a change")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != src {
		t.Fatalf("file rewritten despite satisfied contract: %q", data)
	}
}

func TestEnsureClaudeContractEmptyCwdSkipsTrust(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	changed, err := ensureClaudeContract(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("global flags must still be written")
	}
	var cfg map[string]any
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["projects"]; ok {
		t.Fatalf("no projects entry expected without a cwd: %v", cfg)
	}
}

func TestEnsureClaudeContractInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeContract(p, "/home/u"); err == nil {
		t.Fatal("broken config must error, not be clobbered")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{broken" {
		t.Fatalf("broken config was modified: %q", data)
	}
}

func TestEnsureClaudeContractRejectsNonObjectProjects(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	src := `{"projects": ["weird"]}`
	if err := os.WriteFile(p, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeContract(p, "/home/u"); err == nil {
		t.Fatal("non-object projects must error, not be clobbered")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != src {
		t.Fatalf("unexpected shape was modified: %q", data)
	}
}

func TestWithBypassAccepted(t *testing.T) {
	hooks := `{"hooks":{"Stop":[]}}`
	out := withBypassAccepted(hooks, "bypassPermissions")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("flag not injected: %s", out)
	}
	if _, ok := m["hooks"]; !ok {
		t.Fatalf("hooks payload lost: %s", out)
	}
	if got := withBypassAccepted(hooks, ""); got != hooks {
		t.Fatalf("non-bypass mode must pass settings through, got %s", got)
	}
	if got := withBypassAccepted("not-json", "bypassPermissions"); got != "not-json" {
		t.Fatalf("malformed settings must pass through, got %s", got)
	}
}
