package turnaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
)

func TestTurnIDStableVector(t *testing.T) {
	const want = "66f1ecf04afa693c8fd5c6a9b71cb4ef0e769c40"
	if got := TurnID("user:ivan", 8, 42); got != want {
		t.Fatalf("TurnID changed: got %q, want %q", got, want)
	}
}

func TestInvokeWritesOneJSONDocumentToStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	cfg := &config.AuditHookConfig{Command: []string{"/bin/sh", "-c", `cp /dev/stdin "$1"`, "audit", path}}
	want := Event{
		Schema: Schema, Event: "turn.start",
		Turn: Turn{ID: "66f1ecf04afa693c8fd5c6a9b71cb4ef0e769c40", Seq: 42},
	}
	if err := Invoke(t.Context(), cfg, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("hook stdin is not JSON: %v\n%s", err, raw)
	}
	if got.Schema != want.Schema || got.Event != want.Event || got.Turn.ID != want.Turn.ID {
		t.Fatalf("hook event = %+v, want %+v", got, want)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("hook stdin must end in newline: %q", raw)
	}
}
