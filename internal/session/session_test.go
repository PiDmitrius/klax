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
		def.Sandbox = "on"
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
	if sess.Sandbox != "on" {
		t.Fatalf("sandbox = %q, want on", sess.Sandbox)
	}

	store.UpdateScopeDefaults("user:claw", func(def *ScopeDefaults) {
		def.Backend = "claude"
		def.Model = "sonnet"
		def.Think = "medium"
		def.Sandbox = "off"
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
	if sess.Sandbox != "on" {
		t.Fatalf("snapshot sandbox changed to %q", sess.Sandbox)
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
		sess := store.New("user:claw", "s", "/tmp", defaults)
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
	sess := store.New("user:claw", "main", "/tmp", ScopeDefaults{Backend: "claude"})

	got := store.Get("user:claw", sess.Created)
	if got == nil {
		t.Fatal("Get returned nil for existing session")
	}
	if got.Created != sess.Created || got.Name != sess.Name {
		t.Fatalf("Get returned wrong session: %+v", got)
	}

	got.Name = "mutated"
	again := store.Get("user:claw", sess.Created)
	if again.Name != "main" {
		t.Fatalf("Get must return a clone, got mutation leak: %q", again.Name)
	}

	if store.Get("user:claw", sess.Created+999) != nil {
		t.Fatal("Get must return nil for unknown Created")
	}
}

func TestLoadStoreKeepsEmptyScopeDefaultsAsExplicitDefault(t *testing.T) {
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
          "model_override": "gpt-5.4-mini",
          "think_override": "high",
          "messages": 1
        }
      ]
    }
  },
  "scope_defaults": {
    "user:claw": {
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

	def := store.ScopeDefaults("user:claw")
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
