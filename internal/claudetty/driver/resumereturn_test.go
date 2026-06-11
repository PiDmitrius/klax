package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureResumeReturnDismissedCreatesMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	changed, err := ensureResumeReturnDismissed(p)
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
	if cfg["resumeReturnDismissed"] != true {
		t.Fatalf("flag not set: %v", cfg)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("new config must be 0600, got %v", fi.Mode().Perm())
	}
}

func TestEnsureResumeReturnDismissedMergesPreservingContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	// 9007199254740993 exceeds 2^53: it survives only if numbers round-trip
	// as json.Number, not float64.
	src := `{"numStartups":9007199254740993,"projects":{"/x":{"history":["a"]}},"resumeReturnDismissed":false}`
	if err := os.WriteFile(p, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureResumeReturnDismissed(p)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("flag=false must be rewritten to true")
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
	if cfg["resumeReturnDismissed"] != true {
		t.Fatalf("flag not set: %v", cfg)
	}
	if _, ok := cfg["projects"].(map[string]any)["/x"]; !ok {
		t.Fatalf("existing keys lost: %s", data)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Fatalf("existing perms must be preserved, got %v", fi.Mode().Perm())
	}
}

func TestEnsureResumeReturnDismissedIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	// Deliberately non-canonical formatting: an idempotent call must leave
	// the bytes untouched, proving it didn't rewrite the file.
	src := "{\"resumeReturnDismissed\": true,\t\"k\": 1}"
	if err := os.WriteFile(p, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureResumeReturnDismissed(p)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("flag already true must not report a change")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != src {
		t.Fatalf("file rewritten despite flag set: %q", data)
	}
}

func TestEnsureResumeReturnDismissedInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureResumeReturnDismissed(p); err == nil {
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
