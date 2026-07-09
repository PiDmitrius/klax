package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStoreMigratesLegacyEffortOverrideAndScopeDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "chats": {
    "user:alice": {
      "sessions": [
        {
          "name": "main",
          "cwd": "/tmp/project",
          "active": true,
          "backend": "codex",
          "model_override": "gpt-5.4",
          "effort_override": "high",
          "messages": 1
        }
      ]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(data), 0600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	sess := store.Active("user:alice")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.ThinkOverride != "high" {
		t.Fatalf("ThinkOverride = %q, want high", sess.ThinkOverride)
	}

	def := store.ScopeDefaults("user:alice")
	if def == nil {
		t.Fatal("expected scope defaults")
	}
	if def.Backend != "codex" {
		t.Fatalf("defaults backend = %q, want codex", def.Backend)
	}
	if def.Model != "" {
		t.Fatalf("defaults model = %q, want empty default", def.Model)
	}
	if def.Think != "" {
		t.Fatalf("defaults think = %q, want empty default", def.Think)
	}
}

func TestLoadStorePinsLegacyUsedSessionsToClaude(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "chats": {
    "user:alice": {
      "sessions": [
        {
          "name": "legacy-used",
          "cwd": "/tmp/project",
          "active": true,
          "messages": 7
        }
      ]
    }
  },
  "scope_defaults": {
    "user:alice": {
      "backend": "codex"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(data), 0600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	sess := store.Active("user:alice")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.Backend != "claude" {
		t.Fatalf("backend = %q, want claude", sess.Backend)
	}
}

func TestLoadStoreSupportsLegacyFlatFormat(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "sessions": [
    {
      "name": "legacy",
      "cwd": "/tmp/legacy",
      "active": true,
      "backend": "claude",
      "messages": 0
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(data), 0600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	sessions := store.SessionsFor("_migrated")
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].Name != "legacy" {
		t.Fatalf("name = %q, want legacy", sessions[0].Name)
	}
}

// TestReadThroughWatermarkRoundTrips locks the durable unread cursor: it loads from a store,
// defaults to the zero watermark on a legacy store that omits the
// fields, and survives a Save→reload — including a 0 block index under `omitempty`.
func TestReadThroughWatermarkRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	// First session carries a watermark; the second (legacy) omits the fields entirely.
	data := `{
  "chats": {
    "user:alice": {
      "sessions": [
        {"name": "with-mark", "cwd": "/tmp/p", "active": true, "created": 1000, "messages": 2,
         "read_through_turn": 5, "read_through_block": 3},
        {"name": "legacy", "cwd": "/tmp/p", "created": 2000, "messages": 0}
      ]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(data), 0600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	sessions := store.SessionsFor("user:alice")
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2", len(sessions))
	}
	if sessions[0].ReadThroughTurn != 5 || sessions[0].ReadThroughBlock != 3 {
		t.Fatalf("loaded watermark = (%d,%d), want (5,3)", sessions[0].ReadThroughTurn, sessions[0].ReadThroughBlock)
	}
	// Backward compat: a store predating the fields loads as the zero watermark ("nothing read").
	if sessions[1].ReadThroughTurn != 0 || sessions[1].ReadThroughBlock != 0 {
		t.Fatalf("legacy watermark = (%d,%d), want (0,0)", sessions[1].ReadThroughTurn, sessions[1].ReadThroughBlock)
	}

	// Serialization round-trip: raise the watermark, persist, reload — it survives, and a 0 block
	// index (dropped by omitempty) still reloads as 0 rather than corrupting the turn.
	store.UpdateSession("user:alice", 1000, func(cur *Session) {
		cur.ReadThroughTurn = 9
		cur.ReadThroughBlock = 0
	})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore (reload): %v", err)
	}
	got := reloaded.Get("user:alice", 1000)
	if got == nil {
		t.Fatal("expected session created=1000 after reload")
	}
	if got.ReadThroughTurn != 9 || got.ReadThroughBlock != 0 {
		t.Fatalf("round-tripped watermark = (%d,%d), want (9,0)", got.ReadThroughTurn, got.ReadThroughBlock)
	}
}

func TestNewSnapshotsScopeDefaults(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}

	def := store.EnsureScopeDefaults("user:alice", ScopeDefaults{Backend: "claude"})
	if def.Backend != "claude" {
		t.Fatalf("defaults backend = %q, want claude", def.Backend)
	}
	store.UpdateScopeDefaults("user:alice", func(def *ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.4"
		def.Think = "high"
		def.Sandbox = "on"
		def.ClaudeTTY = true
	})

	sess := store.New("user:alice", "main", "/tmp/project", *store.ScopeDefaults("user:alice"))
	if sess.Backend != "codex" {
		t.Fatalf("backend = %q, want codex", sess.Backend)
	}
	if sess.ModelOverride != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", sess.ModelOverride)
	}
	if sess.ThinkOverride != "high" {
		t.Fatalf("think = %q, want high", sess.ThinkOverride)
	}
	if sess.Sandbox != "on" {
		t.Fatalf("sandbox = %q, want on", sess.Sandbox)
	}
	if !sess.ClaudeTTY {
		t.Fatal("claude tty default was not copied to new session")
	}

	store.UpdateScopeDefaults("user:alice", func(def *ScopeDefaults) {
		def.Backend = "claude"
		def.Model = "sonnet"
		def.Think = "medium"
		def.Sandbox = "off"
		def.ClaudeTTY = false
	})

	sess = store.Active("user:alice")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.Backend != "codex" {
		t.Fatalf("snapshot backend changed to %q", sess.Backend)
	}
	if sess.ModelOverride != "gpt-5.4" {
		t.Fatalf("snapshot model changed to %q", sess.ModelOverride)
	}
	if sess.ThinkOverride != "high" {
		t.Fatalf("snapshot think changed to %q", sess.ThinkOverride)
	}
	if sess.Sandbox != "on" {
		t.Fatalf("snapshot sandbox changed to %q", sess.Sandbox)
	}
	if !sess.ClaudeTTY {
		t.Fatal("snapshot claude tty changed")
	}
}

func TestNewAssignsUniqueCreatedAcrossRapidCalls(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	defaults := ScopeDefaults{Backend: "claude"}

	const n = 5
	seen := make(map[int64]bool, n)
	for i := 0; i < n; i++ {
		sess := store.New("user:alice", "s", "/tmp", defaults)
		if seen[sess.Created] {
			t.Fatalf("duplicate Created %d on iteration %d", sess.Created, i)
		}
		seen[sess.Created] = true
	}
}

func TestGetReturnsCloneByCreated(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	sess := store.New("user:alice", "main", "/tmp", ScopeDefaults{Backend: "claude"})

	got := store.Get("user:alice", sess.Created)
	if got == nil {
		t.Fatal("Get returned nil for existing session")
	}
	if got.Created != sess.Created || got.Name != sess.Name {
		t.Fatalf("Get returned wrong session: %+v", got)
	}

	got.Name = "mutated"
	again := store.Get("user:alice", sess.Created)
	if again.Name != "main" {
		t.Fatalf("Get must return a clone, got mutation leak: %q", again.Name)
	}

	if store.Get("user:alice", sess.Created+999) != nil {
		t.Fatal("Get must return nil for unknown Created")
	}
}

func TestLoadStoreKeepsEmptyScopeDefaultsAsExplicitDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "chats": {
    "user:alice": {
      "sessions": [
        {
          "name": "main",
          "cwd": "/tmp/project",
          "active": true,
          "backend": "codex",
          "model_override": "gpt-5.4-mini",
          "think_override": "high",
          "messages": 1
        }
      ]
    }
  },
  "scope_defaults": {
    "user:alice": {
      "backend": "codex",
      "model": "",
      "think": ""
    }
  }
}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(data), 0600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	def := store.ScopeDefaults("user:alice")
	if def == nil {
		t.Fatal("expected scope defaults")
	}
	if def.Model != "" {
		t.Fatalf("defaults model = %q, want explicit empty default", def.Model)
	}
	if def.Think != "" {
		t.Fatalf("defaults think = %q, want explicit empty default", def.Think)
	}
}
