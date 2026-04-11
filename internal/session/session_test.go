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
    "user:claw": {
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

	sess := store.Active("user:claw")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.ThinkOverride != "high" {
		t.Fatalf("ThinkOverride = %q, want high", sess.ThinkOverride)
	}

	def := store.ScopeDefaults("user:claw")
	if def == nil {
		t.Fatal("expected scope defaults")
	}
	if def.Backend != "codex" {
		t.Fatalf("defaults backend = %q, want codex", def.Backend)
	}
	if def.Model != "gpt-5.4" {
		t.Fatalf("defaults model = %q, want gpt-5.4", def.Model)
	}
	if def.Think != "high" {
		t.Fatalf("defaults think = %q, want high", def.Think)
	}
}

func TestLoadStorePinsLegacyUsedSessionsToClaude(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "chats": {
    "user:claw": {
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
    "user:claw": {
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

	sess := store.Active("user:claw")
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

func TestNewSnapshotsScopeDefaults(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}

	def := store.EnsureScopeDefaults("user:claw", ScopeDefaults{Backend: "claude"})
	if def.Backend != "claude" {
		t.Fatalf("defaults backend = %q, want claude", def.Backend)
	}
	store.UpdateScopeDefaults("user:claw", func(def *ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.4"
		def.Think = "high"
	})

	sess := store.New("user:claw", "main", "/tmp/project", *store.ScopeDefaults("user:claw"))
	if sess.Backend != "codex" {
		t.Fatalf("backend = %q, want codex", sess.Backend)
	}
	if sess.ModelOverride != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", sess.ModelOverride)
	}
	if sess.ThinkOverride != "high" {
		t.Fatalf("think = %q, want high", sess.ThinkOverride)
	}

	store.UpdateScopeDefaults("user:claw", func(def *ScopeDefaults) {
		def.Backend = "claude"
		def.Model = "claude-sonnet-4-6[1m]"
		def.Think = "medium"
	})

	sess = store.Active("user:claw")
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
}
