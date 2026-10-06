package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStorePinsLegacyUsedSessionsToClaude(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", tmp)

	data := `{
  "chats": {
    "user:alice": {
      "sessions": [
        {
          "klax_id": "a1",
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

// The durable read position survives Save→reload, including a 0 block, and never moves back.
func TestReadPosRoundTrips(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	sess := store.New("user:alice", "s", "/tmp/p", ScopeDefaults{})
	if turn, block := sess.ReadPosition(false); turn != 0 || block != 0 {
		t.Fatalf("fresh position = %d.%d", turn, block)
	}
	store.UpdateSession("user:alice", sess.KlaxID, func(cur *Session) {
		if !cur.AdvanceReadPos(false, 9, 0) || !cur.AdvanceReadPos(true, 4, 2) || cur.AdvanceReadPos(false, 8, 5) {
			t.Fatal("AdvanceReadPos must raise and never lower")
		}
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Get("user:alice", sess.KlaxID)
	if got == nil || got.ReadPos != "9.0" || got.ReadPosRO != "4.2" {
		t.Fatalf("reloaded = %+v", got)
	}
	for _, bad := range []string{"9", "a.1", "1.-1", "1.2.3"} {
		if _, _, err := ParseReadPos(bad); err == nil {
			t.Fatalf("ParseReadPos(%q) accepted", bad)
		}
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
		def.ModelRequested = "gpt-5.6-sol"
		def.Think = "high"
		def.Sandbox = "on"
		def.TTY = true
	})

	sess := store.New("user:alice", "main", "/tmp/project", *store.ScopeDefaults("user:alice"))
	if sess.Backend != "codex" {
		t.Fatalf("backend = %q, want codex", sess.Backend)
	}
	if sess.ModelRequested != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", sess.ModelRequested)
	}
	if sess.Think != "high" {
		t.Fatalf("think = %q, want high", sess.Think)
	}
	if sess.Sandbox != "on" {
		t.Fatalf("sandbox = %q, want on", sess.Sandbox)
	}
	if !sess.TTY {
		t.Fatal("claude tty default was not copied to new session")
	}

	store.UpdateScopeDefaults("user:alice", func(def *ScopeDefaults) {
		def.Backend = "claude"
		def.ModelRequested = "sonnet"
		def.Think = "medium"
		def.Sandbox = "off"
		def.TTY = false
	})

	sess = store.Active("user:alice")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.Backend != "codex" {
		t.Fatalf("snapshot backend changed to %q", sess.Backend)
	}
	if sess.ModelRequested != "gpt-5.6-sol" {
		t.Fatalf("snapshot model changed to %q", sess.ModelRequested)
	}
	if sess.Think != "high" {
		t.Fatalf("snapshot think changed to %q", sess.Think)
	}
	if sess.Sandbox != "on" {
		t.Fatalf("snapshot sandbox changed to %q", sess.Sandbox)
	}
	if !sess.TTY {
		t.Fatal("snapshot claude tty changed")
	}
}

func TestNewAssignsUniqueKlaxIDAcrossRapidCalls(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	defaults := ScopeDefaults{Backend: "claude"}

	const n = 5
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		sess := store.New("user:alice", "s", "/tmp", defaults)
		if seen[sess.KlaxID] {
			t.Fatalf("duplicate klax_id %s on iteration %d", sess.KlaxID, i)
		}
		seen[sess.KlaxID] = true
	}
}

func TestSessionKeysStayUniqueAcrossMerge(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	a := store.New("tg:1", "telegram", "/tmp", ScopeDefaults{})
	b := store.New("mx:1", "max", "/tmp", ScopeDefaults{})
	c := store.New("user:alice", "canonical", "/tmp", ScopeDefaults{})
	if a.KlaxID == b.KlaxID || b.KlaxID == c.KlaxID || a.KlaxID == c.KlaxID {
		t.Fatalf("keys must be unique across chats; got %s,%s,%s", a.KlaxID, b.KlaxID, c.KlaxID)
	}
	if merged, err := store.MergeKeys("user:alice", []string{"tg:1", "mx:1"}); err != nil || !merged {
		t.Fatalf("MergeKeys = %v, %v", merged, err)
	}
	seen := map[string]bool{}
	for _, sess := range store.SessionsFor("user:alice") {
		if seen[sess.KlaxID] {
			t.Fatalf("duplicate key %s after merge", sess.KlaxID)
		}
		seen[sess.KlaxID] = true
	}
	if next := store.New("user:alice", "next", "/tmp", ScopeDefaults{}); seen[next.KlaxID] {
		t.Fatalf("next key after merge %s repeats an existing one", next.KlaxID)
	}
}

func TestKeyMigrationResumesAfterFailedSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", dir)
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	sess := store.New("tg:1", "pending", "/tmp", ScopeDefaults{})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	src, dst := WorkDir("tg:1", sess.KlaxID), WorkDir("user:test", sess.KlaxID)
	if err := os.MkdirAll(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "queue.jsonl"), []byte("durable"), 0600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "sessions.saved.json")
	if err := os.Rename(store.path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.path, 0700); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MergeKeys("user:test", []string{"tg:1"}); err == nil || changed {
		t.Fatalf("failed save accepted: %v, %v", changed, err)
	}
	if store.Get("tg:1", sess.KlaxID) == nil || store.Get("user:test", sess.KlaxID) != nil {
		t.Fatal("metadata changed after failed save")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, store.path); err != nil {
		t.Fatal(err)
	}
	store, err = LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MergeKeys("user:test", []string{"tg:1"}); err != nil || !changed {
		t.Fatalf("retry = %v, %v", changed, err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "queue.jsonl"))
	if err != nil || string(data) != "durable" {
		t.Fatalf("migrated data = %q, %v", data, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
}

func TestKeyMigrationRefusesConflictingDirectories(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	sess := store.New("tg:1", "pending", "/tmp", ScopeDefaults{})
	for key, text := range map[string]string{"tg:1": "source", "user:test": "destination"} {
		path := WorkDir(key, sess.KlaxID)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "queue.jsonl"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := store.MergeKeys("user:test", []string{"tg:1"}); err == nil || changed {
		t.Fatalf("conflict ignored: %v, %v", changed, err)
	}
	if store.Get("tg:1", sess.KlaxID) == nil || store.Get("user:test", sess.KlaxID) != nil {
		t.Fatal("conflicting migration changed metadata")
	}
	for key, text := range map[string]string{"tg:1": "source", "user:test": "destination"} {
		data, err := os.ReadFile(filepath.Join(WorkDir(key, sess.KlaxID), "queue.jsonl"))
		if err != nil || string(data) != text {
			t.Fatalf("conflict overwrote %s: %q, %v", key, data, err)
		}
	}
}

func TestAddInsertsFullyFormedSessionActivating(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	first := store.New("user:alice", "one", "/tmp", ScopeDefaults{Backend: "claude"})
	// Add a fully-formed session in one atomic op; it must become active and deactivate the previous.
	added := store.Add("user:alice", &Session{Name: "two", Backend: "codex", ModelRequested: "m", CWD: "/w"})
	if added.KlaxID == "" || added.KlaxID == first.KlaxID {
		t.Fatalf("Add must assign a unique klax_id: %s vs %s", added.KlaxID, first.KlaxID)
	}
	if !added.Active {
		t.Fatal("added session must be active")
	}
	if added.Name != "two" || added.Backend != "codex" || added.ModelRequested != "m" || added.CWD != "/w" {
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
	if store.Active("user:alice").KlaxID != added.KlaxID {
		t.Fatal("the added session must be the active one")
	}
}

func TestAddWithDefaultsCommitsSessionAndTemplateTogether(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	def := &ScopeDefaults{Backend: "codex", ModelRequested: "m", Think: "high", Sandbox: "on"}
	added := store.AddWithDefaults("user:alice", &Session{Name: "new", CWD: "/tmp", Backend: "codex"}, def)
	if added == nil || added.KlaxID == "" {
		t.Fatalf("session was not added: %+v", added)
	}
	got := store.ScopeDefaults("user:alice")
	if got == nil || *got != *def {
		t.Fatalf("defaults = %+v, want %+v", got, def)
	}
}

func TestScopeDefaultsReturnsIndependentGroupState(t *testing.T) {
	enabled, verbose := true, false
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	store.UpdateScopeDefaults("ym:group#thread", func(def *ScopeDefaults) {
		def.GroupMode = &enabled
		def.GroupVerbose = &verbose
		def.GroupAttachmentMode = "any"
	})

	got := store.ScopeDefaults("ym:group#thread")
	if got.GroupMode == nil || got.GroupVerbose == nil || got.GroupAttachmentMode != "any" {
		t.Fatalf("group state missing from clone: %+v", got)
	}
	*got.GroupMode = false
	*got.GroupVerbose = true

	again := store.ScopeDefaults("ym:group#thread")
	if !*again.GroupMode || *again.GroupVerbose || again.GroupAttachmentMode != "any" {
		t.Fatalf("mutating a clone changed stored defaults: %+v", again)
	}
}

func TestReorderRearrangesAndToleratesPartialOrder(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, store.New("user:alice", "s", "/tmp", ScopeDefaults{Backend: "claude"}).KlaxID)
	}
	// ids is [a,b,c,d] in creation order. A filtered (group) strip showing only c and d drags them
	// into the order [d,c]: the SLOTS they occupied (2 and 3) are refilled, and a/b never move.
	if !store.Reorder("user:alice", []string{ids[3], ids[2]}) {
		t.Fatal("Reorder returned false for a real change")
	}
	got := store.SessionsFor("user:alice")
	want := []string{ids[0], ids[1], ids[3], ids[2]}
	for i, w := range want {
		if got[i].KlaxID != w {
			t.Fatalf("Reorder order[%d]=%s, want %s (full: %v)", i, got[i].KlaxID, w, klaxIDs(got))
		}
	}
	// An unknown id is ignored and a no-op order changes nothing.
	if store.Reorder("user:alice", []string{"unknown"}) {
		t.Fatal("Reorder must be a no-op (false) when nothing moves")
	}
	if got2 := store.SessionsFor("user:alice"); klaxIDs(got2)[0] != ids[0] {
		t.Fatalf("no-op Reorder disturbed the order: %v", klaxIDs(got2))
	}
	// A FULL list is the same operation with every slot occupied: the result is exactly the request,
	// so the unfiltered root strip keeps behaving as it always did.
	if !store.Reorder("user:alice", []string{ids[2], ids[0], ids[3], ids[1]}) {
		t.Fatal("Reorder returned false for a real full-list change")
	}
	if got3 := klaxIDs(store.SessionsFor("user:alice")); got3[0] != ids[2] || got3[1] != ids[0] || got3[2] != ids[3] || got3[3] != ids[1] {
		t.Fatalf("full-list Reorder did not apply verbatim: %v", got3)
	}
}

// A group drag must not disturb a DIFFERENT group whose members it does not share — the guarantee
// that lets one global order serve every filtered view.
func TestReorderWithinOneGroupLeavesDisjointGroupUntouched(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, store.New("user:alice", "s", "/tmp", ScopeDefaults{Backend: "claude"}).KlaxID)
	}
	// Interleaved membership: x = {a, c}, y = {b, d}.
	store.Reorder("user:alice", []string{ids[2], ids[0]}) // drag inside x: c before a
	got := klaxIDs(store.SessionsFor("user:alice"))
	want := []string{ids[2], ids[1], ids[0], ids[3]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slot-preserving order = %v, want %v", got, want)
		}
	}
	// y's members kept both their absolute positions and their relative order.
	if got[1] != ids[1] || got[3] != ids[3] {
		t.Fatalf("a disjoint group's members moved: %v", got)
	}
}

func TestNormalizeGroups(t *testing.T) {
	got, err := NormalizeGroups([]string{" work ", "work", "", "дом"})
	if err != nil {
		t.Fatalf("NormalizeGroups: %v", err)
	}
	if len(got) != 2 || got[0] != "work" || got[1] != "дом" {
		t.Fatalf("NormalizeGroups = %v, want [work дом]", got)
	}
	if got, err := NormalizeGroups([]string{"  "}); err != nil || got != nil {
		t.Fatalf("blank-only groups = %v, %v; want nil, nil", got, err)
	}
	// The rejections that keep `#<group>` unambiguous against `#/<klax_id>` and `#is:unread`, plus
	// the root pseudo-group and the two bounds.
	if _, err := NormalizeGroups([]string{"123"}); err != nil {
		t.Fatalf("an all-digit group name is unambiguous now: %v", err)
	}
	bad := []string{"is:unread", "a/b", "a#b", "*", "a\u0000b", strings.Repeat("x", MaxGroupLen+1)}
	for _, name := range bad {
		if _, err := NormalizeGroups([]string{name}); err == nil {
			t.Fatalf("NormalizeGroups(%q) accepted an unusable name", name)
		}
	}
	many := make([]string, 0, MaxGroupCount+1)
	for i := 0; i <= MaxGroupCount; i++ {
		many = append(many, "g"+string(rune('a'+i)))
	}
	if _, err := NormalizeGroups(many); err == nil {
		t.Fatalf("NormalizeGroups accepted %d groups on one session", len(many))
	}
	// A name at the limit is fine — the bound is inclusive.
	if _, err := NormalizeGroups([]string{strings.Repeat("x", MaxGroupLen)}); err != nil {
		t.Fatalf("NormalizeGroups rejected a name of exactly MaxGroupLen: %v", err)
	}
}

// A returned session must not share its group slice with the store, or a caller's append/edit would
// silently mutate stored state.
func TestGetReturnsDetachedGroups(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	c := store.New("user:alice", "s", "/tmp", ScopeDefaults{}).KlaxID
	store.UpdateSession("user:alice", c, func(cur *Session) { cur.Groups = []string{"work"} })
	got := store.Get("user:alice", c)
	got.Groups[0] = "hacked"
	if again := store.Get("user:alice", c); again.Groups[0] != "work" {
		t.Fatalf("stored groups were mutated through a returned copy: %v", again.Groups)
	}
}

func klaxIDs(ss []*Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.KlaxID
	}
	return out
}

func TestGetReturnsCloneByKlaxID(t *testing.T) {
	store := &Store{
		Chats: make(map[string]*ChatSessions),
		Scope: make(map[string]*ScopeDefaults),
	}
	sess := store.New("user:alice", "main", "/tmp", ScopeDefaults{Backend: "claude"})

	got := store.Get("user:alice", sess.KlaxID)
	if got == nil {
		t.Fatal("Get returned nil for existing session")
	}
	if got.KlaxID != sess.KlaxID || got.Name != sess.Name {
		t.Fatalf("Get returned wrong session: %+v", got)
	}

	got.Name = "mutated"
	again := store.Get("user:alice", sess.KlaxID)
	if again.Name != "main" {
		t.Fatalf("Get must return a clone, got mutation leak: %q", again.Name)
	}

	if store.Get("user:alice", "unknown") != nil {
		t.Fatal("Get must return nil for an unknown klax_id")
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
          "klax_id": "a1",
          "name": "main",
          "cwd": "/tmp/project",
          "active": true,
          "backend": "codex",
          "model_requested": "gpt-5.5",
          "think": "high",
          "messages": 1
        }
      ]
    }
  },
  "scope_defaults": {
    "user:alice": {
      "backend": "codex",
      "model_requested": "",
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
	if def.ModelRequested != "" {
		t.Fatalf("defaults model = %q, want explicit empty default", def.ModelRequested)
	}
	if def.Think != "" {
		t.Fatalf("defaults think = %q, want explicit empty default", def.Think)
	}
}

func TestSetCWDIfMessages0RejectsWhenMessagesAlreadyStarted(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	sess := store.New("tg:1", "one", "/original", ScopeDefaults{})
	store.UpdateActive("tg:1", func(s *Session) { s.Messages = 1 })

	got, ok := store.SetCWDIfMessages0("tg:1", sess.KlaxID, "/new")

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

	got, ok := store.SetCWDIfMessages0("tg:1", sess.KlaxID, "/new")

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
		"tg:1", sess.KlaxID,
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
		"tg:1", sess.KlaxID,
		func(*Session) error { return nil },
		func(s *Session) { s.CWD = "/new" },
	)

	if err != nil || got == nil || got.CWD != "/new" {
		t.Fatalf("got = %+v, err = %v, want CWD=/new, nil err", got, err)
	}
}

func TestUpdateSessionCheckedReturnsErrSessionNotFoundForMissingKlaxID(t *testing.T) {
	store := &Store{Chats: map[string]*ChatSessions{}, Scope: map[string]*ScopeDefaults{}}
	store.New("tg:1", "one", "/tmp", ScopeDefaults{})

	_, err := store.UpdateSessionChecked("tg:1", "unknown", nil, func(*Session) {})

	if err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}
