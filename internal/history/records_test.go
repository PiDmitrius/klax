package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/PiDmitrius/klax/internal/promptcanon"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestScanRecordsPhysicalCoordinates(t *testing.T) {
	scan := func(data string) ([]rawRecord, int64) {
		var recs []rawRecord
		n, err := scanRecords(strings.NewReader(data), 0, func(r rawRecord) {
			r.Raw = slices.Clone(r.Raw)
			recs = append(recs, r)
		})
		if err != nil {
			t.Fatal(err)
		}
		return recs, n
	}
	recs, n := scan("ok\r\n\n{bad}\ntorn")
	if len(recs) != 3 || n != int64(len("ok\r\n\n{bad}\n")) {
		t.Fatalf("records = %d, consumed %d", len(recs), n)
	}
	for i, r := range recs {
		if r.Event != int64(i) {
			t.Fatalf("event[%d]=%d", i, r.Event)
		}
	}
	if string(recs[1].Raw) != "" || recs[0].Len != 4 {
		t.Fatalf("blank raw = %q, first len %d", recs[1].Raw, recs[0].Len)
	}
	s := sha256.Sum256([]byte("ok"))
	if recs[0].digest() != hex.EncodeToString(s[:]) {
		t.Fatal("raw digest mismatch")
	}
	recs, _ = scan("ok\n\n{bad}\ntorn\n")
	if len(recs) != 4 || string(recs[3].Raw) != "torn" {
		t.Fatal("completed tail was not admitted")
	}
}

func TestExternalUserDigestPreservesTrailingNewline(t *testing.T) {
	prompt := "  привет\nline two\n"
	raw := []byte(`{"message":{"content":"  привет\nline two\n"}}`)
	if got := claudeUserDigest(raw); got != promptcanon.Digest(prompt) {
		t.Fatalf("Claude digest = %s", got)
	}
	if promptcanon.Digest(prompt) == promptcanon.Digest("  привет\nline two") {
		t.Fatal("trailing newline was discarded")
	}
}

func TestSnapshotRequiresKnownTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, _, err := Snapshot("codex", "missing-session", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Snapshot error = %v", err)
	}
	if items, err := Load("codex", "missing-session", ""); err != nil || items != nil {
		t.Fatalf("Load compatibility = %+v, %v", items, err)
	}
}

func TestClaudeUserPayloadStringAndBlocks(t *testing.T) {
	for _, raw := range []string{
		`{"message":{"content":" hello\n"}}`,
		`{"message":{"content":[{"type":"text","text":" hello"},{"type":"tool_result","text":"ignore"},{"type":"text","text":"\n"}]}}`,
	} {
		got, ok := claudeUserPayload([]byte(raw))
		if !ok || got != " hello\n" {
			t.Fatalf("payload = %q, %v", got, ok)
		}
		if claudeUserDigest([]byte(raw)) != promptcanon.Digest(" hello\n") {
			t.Fatal("Claude payload digest differs from submitted prompt")
		}
	}
}

func TestCodexUserPayloadDigestMatchesSubmittedPrompt(t *testing.T) {
	prompt := "caption\n\nПрикреплённые файлы:\n/tmp/klax-attach-42/a.txt\n"
	b, _ := json.Marshal(map[string]any{"timestamp": "2026-07-18T00:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": prompt}})
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := readCodex(path)
	if err != nil || len(items) != 1 {
		t.Fatalf("Codex fixture = %+v, %v", items, err)
	}
	if items[0].PromptDigest != promptcanon.Digest(prompt) {
		t.Fatal("Codex payload digest differs from submitted prompt")
	}
}
