package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
          "model_override": "gpt-5.6-sol",
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
		def.Model = "gpt-5.6-sol"
		def.Think = "high"
		def.Sandbox = "on"
		def.ClaudeTTY = true
	})

	sess := store.New("user:alice", "main", "/tmp/project", *store.ScopeDefaults("user:alice"))
	if sess.Backend != "codex" {
		t.Fatalf("backend = %q, want codex", sess.Backend)
	}
	if sess.ModelOverride != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", sess.ModelOverride)
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
	if sess.ModelOverride != "gpt-5.6-sol" {
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

// The session key is a store-global monotonic counter that is NEVER reused after a delete (reuse would
// bind a new session to a deleted one's removed durable Store), and the high-water survives a
// save/reload. Invariant: 1, 2, delete → 3 (not 2).
func TestCreatedNeverReusedAfterDelete(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	a := store.New("user:alice", "a", "/tmp", ScopeDefaults{})
	b := store.New("user:alice", "b", "/tmp", ScopeDefaults{})
	if a.Created != 1 || b.Created != 2 {
		t.Fatalf("keys must count 1,2, got a=%d b=%d", a.Created, b.Created)
	}
	// Delete the highest (b) — the high-water must NOT drop, so the next key is 3, not a reused 2.
	if !store.Delete("user:alice", 1) {
		t.Fatal("delete failed")
	}
	c := store.New("user:alice", "c", "/tmp", ScopeDefaults{})
	if c.Created != 3 {
		t.Fatalf("key reused after delete: want 3, got %d", c.Created)
	}
	// The high-water survives a save + reload.
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	if d := reloaded.New("user:alice", "d", "/tmp", ScopeDefaults{}); d.Created != 4 {
		t.Fatalf("high-water not persisted across reload: want 4, got %d", d.Created)
	}
}

func TestSessionKeysStayUniqueAcrossMerge(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	a := store.New("tg:1", "telegram", "/tmp", ScopeDefaults{})
	b := store.New("mx:1", "max", "/tmp", ScopeDefaults{})
	c := store.New("user:alice", "canonical", "/tmp", ScopeDefaults{})
	if a.Created != 1 || b.Created != 2 || c.Created != 3 {
		t.Fatalf("keys must be global 1,2,3; got %d,%d,%d", a.Created, b.Created, c.Created)
	}
	if !store.MergeKeys("user:alice", []string{"tg:1", "mx:1"}) {
		t.Fatal("MergeKeys returned false")
	}
	seen := map[int64]bool{}
	for _, sess := range store.SessionsFor("user:alice") {
		if seen[sess.Created] {
			t.Fatalf("duplicate key %d after merge", sess.Created)
		}
		seen[sess.Created] = true
	}
	if next := store.New("user:alice", "next", "/tmp", ScopeDefaults{}); next.Created != 4 {
		t.Fatalf("next key after merge = %d, want 4", next.Created)
	}
}

func TestLoadMigratesPerChatHighWaterToGlobal(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	path := filepath.Join(StoreDir(), "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"chats":{"user:alice":{"high_water":9,"sessions":[]},"tg:2":{"high_water":12,"sessions":[]}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := store.New("user:alice", "next", "/tmp", ScopeDefaults{}).Created; got != 13 {
		t.Fatalf("key after per-chat high-water migration = %d, want 13", got)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(saved), `"high_water"`) != 1 {
		t.Fatalf("saved store must contain only the global high-water: %s", saved)
	}
}

func TestAddInsertsFullyFormedSessionActivating(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	first := store.New("user:alice", "one", "/tmp", ScopeDefaults{Backend: "claude"})
	// Add a fully-formed session in one atomic op; it must become active and deactivate the previous.
	added := store.Add("user:alice", &Session{Name: "two", Backend: "codex", ModelOverride: "m", CWD: "/w"})
	if added.Created == 0 || added.Created <= first.Created {
		t.Fatalf("Add must assign a unique increasing Created: %d vs %d", added.Created, first.Created)
	}
	if !added.Active {
		t.Fatal("added session must be active")
	}
	if added.Name != "two" || added.Backend != "codex" || added.ModelOverride != "m" || added.CWD != "/w" {
		t.Fatalf("added session lost its formed config: %+v", added)
	}
	sessions := store.SessionsFor("user:alice")
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}
	active := 0
	for _, s := range sessions {
		if s.Active {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("exactly one session must be active after Add, got %d", active)
	}
	if store.Active("user:alice").Created != added.Created {
		t.Fatal("the added session must be the active one")
	}
}

func TestAddWithDefaultsCommitsSessionAndTemplateTogether(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	def := &ScopeDefaults{Backend: "codex", Model: "m", Think: "high", Sandbox: "on"}
	added := store.AddWithDefaults("user:alice", &Session{Name: "new", CWD: "/tmp", Backend: "codex"}, def)
	if added == nil || added.Created == 0 {
		t.Fatalf("session was not added: %+v", added)
	}
	got := store.ScopeDefaults("user:alice")
	if got == nil || *got != *def {
		t.Fatalf("defaults = %+v, want %+v", got, def)
	}
}

func TestScopeDefaultsReturnsIndependentGroupState(t *testing.T) {
	enabled, verbose, legacyAttachments := true, false, true
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	store.UpdateScopeDefaults("ym:group#thread", func(def *ScopeDefaults) {
		def.GroupMode = &enabled
		def.GroupVerbose = &verbose
		def.GroupAttachmentMode = "any"
		def.LegacyGroupAttachments = &legacyAttachments
	})

	got := store.ScopeDefaults("ym:group#thread")
	if got.GroupMode == nil || got.GroupVerbose == nil || got.LegacyGroupAttachments == nil || got.GroupAttachmentMode != "any" {
		t.Fatalf("group state missing from clone: %+v", got)
	}
	*got.GroupMode = false
	*got.GroupVerbose = true
	*got.LegacyGroupAttachments = false

	again := store.ScopeDefaults("ym:group#thread")
	if !*again.GroupMode || *again.GroupVerbose || !*again.LegacyGroupAttachments || again.GroupAttachmentMode != "any" {
		t.Fatalf("mutating a clone changed stored defaults: %+v", again)
	}
}

func TestReorderRearrangesAndToleratesPartialOrder(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	var ids []int64
	for i := 0; i < 4; i++ {
		ids = append(ids, store.New("user:alice", "s", "/tmp", ScopeDefaults{Backend: "claude"}).Created)
	}
	// ids is [a,b,c,d] in creation order. Move d to the front, c after it.
	if !store.Reorder("user:alice", []int64{ids[3], ids[2]}) {
		t.Fatal("Reorder returned false for a real change")
	}
	got := store.SessionsFor("user:alice")
	// Listed ids come first in the requested order; the rest keep their relative order.
	want := []int64{ids[3], ids[2], ids[0], ids[1]}
	for i, w := range want {
		if got[i].Created != w {
			t.Fatalf("Reorder order[%d]=%d, want %d (full: %v)", i, got[i].Created, w, createds(got))
		}
	}
	// An unknown id is ignored and a no-op order changes nothing.
	if store.Reorder("user:alice", []int64{99999}) {
		t.Fatal("Reorder must be a no-op (false) when nothing moves")
	}
	if got2 := store.SessionsFor("user:alice"); createds(got2)[0] != ids[3] {
		t.Fatalf("no-op Reorder disturbed the order: %v", createds(got2))
	}
}

func createds(ss []*Session) []int64 {
	out := make([]int64, len(ss))
	for i, s := range ss {
		out[i] = s.Created
	}
	return out
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
          "model_override": "gpt-5.5",
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

func TestSetCWDIfMessages0RejectsWhenMessagesAlreadyStarted(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	sess := store.New("tg:1", "one", "/original", ScopeDefaults{})
	store.UpdateActive("tg:1", func(s *Session) { s.Messages = 1 })

	got, ok := store.SetCWDIfMessages0("tg:1", sess.Created, "/new")

	if ok {
		t.Fatal("SetCWDIfMessages0 must refuse once Messages > 0")
	}
	if got == nil || got.CWD != "/original" {
		t.Fatalf("session CWD = %+v, want unchanged /original", got)
	}
	if store.ScopeDefaults("tg:1").CWD == "/new" {
		t.Fatal("ScopeDefaults.CWD must not change when the session-level write was refused")
	}
}

func TestSetCWDIfMessages0SetsSessionAndScopeDefaultsTogether(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	sess := store.New("tg:1", "one", "/original", ScopeDefaults{})

	got, ok := store.SetCWDIfMessages0("tg:1", sess.Created, "/new")

	if !ok || got.CWD != "/new" {
		t.Fatalf("SetCWDIfMessages0 = %+v, %v, want CWD=/new, true", got, ok)
	}
	if store.ScopeDefaults("tg:1").CWD != "/new" {
		t.Fatal("ScopeDefaults.CWD must be set together with the session's CWD")
	}
}

func TestUpdateSessionCheckedSkipsMutationWhenCheckFails(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	sess := store.New("tg:1", "one", "/original", ScopeDefaults{})
	refuse := errors.New("refused")

	got, err := store.UpdateSessionChecked(
		"tg:1", sess.Created,
		func(*Session) error { return refuse },
		func(s *Session) { s.CWD = "/new" },
	)

	if err != refuse {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if got == nil || got.CWD != "/original" {
		t.Fatalf("session = %+v, want unchanged /original (mutation must not run when check fails)", got)
	}
}

func TestUpdateSessionCheckedAppliesMutationWhenCheckPasses(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	sess := store.New("tg:1", "one", "/original", ScopeDefaults{})

	got, err := store.UpdateSessionChecked(
		"tg:1", sess.Created,
		func(*Session) error { return nil },
		func(s *Session) { s.CWD = "/new" },
	)

	if err != nil || got == nil || got.CWD != "/new" {
		t.Fatalf("got = %+v, err = %v, want CWD=/new, nil err", got, err)
	}
}

func TestUpdateSessionCheckedReturnsErrSessionNotFoundForMissingCreated(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	store.New("tg:1", "one", "/tmp", ScopeDefaults{})

	_, err := store.UpdateSessionChecked("tg:1", 999999, nil, func(*Session) {})

	if err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}
