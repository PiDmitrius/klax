package runner

import (
	"os"
	"strings"
	"testing"
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
