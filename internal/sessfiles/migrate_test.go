package sessfiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/ids"
	"github.com/PiDmitrius/klax/internal/session"
)

const legacyStoreJSON = `{
  "high_water": 1783809967,
  "chats": {
    "user:alice": {"sessions": [
      {"id": "89c19fb7", "name": "work", "cwd": "/w", "created": 1775462460, "active": true, "backend": "claude",
       "model": "claude-sonnet-x", "model_override": "sonnet", "think_override": "high", "claude_tty": true,
       "ctx_window": 200000, "ctx_used": 1000, "messages": 3, "groups": ["dev"],
       "read_through_turn": 42, "read_through_block": 3, "reader_read_through_turn": 40, "reader_read_through_block": 1,
       "append_system_prompt": "Answer in Russian.", "rl_status": "allowed"},
      {"id": "", "name": "dup", "cwd": "/w", "created": 1775462460, "effort_override": "low"},
      {"id": "", "name": "nodir", "cwd": "/w", "created": 1783809967}
    ]},
    "mx:1": {"sessions": [{"id": "t1", "name": "mx", "cwd": "/m", "created": 5}]}
  },
  "scope_defaults": {
    "user:alice": {"backend": "claude", "model": "opus", "think": "high", "claude_tty": true, "cwd": "/w"},
    "ym:g#t": {"group_attachments": true}
  }
}`

const legacyQueue = `{"ev":"enq","seq":1,"chat":"ui:alice","text":"hi","ts":1791059329415329941}
{"ev":"run_session","seq":1,"backend":"claude","session":"89c19fb7","from_event":12}
{"ev":"done","seq":1,"ctx_window":200000}
{"ev":"enq","seq":2,"te`

func legacyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(legacyStoreJSON), 0600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		key     string
		created int64
	}{{"user:alice", 1775462460}, {"mx:1", 5}} {
		sd := filepath.Join(dir, "sessions", session.KeyDir(d.key), fmt.Sprint(d.created))
		if err := os.MkdirAll(sd, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sd, "queue.jsonl"), []byte(legacyQueue), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadMigrated(t *testing.T, dir string) (*session.Store, map[string][]migratedSession) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "sessions.migrate.json")); !os.IsNotExist(err) {
		t.Fatalf("journal left behind: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "sessions.migrated.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mapping map[string][]migratedSession
	if err := json.Unmarshal(raw, &mapping); err != nil {
		t.Fatal(err)
	}
	store, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	return store, mapping
}

func checkMigrated(t *testing.T, dir string) {
	t.Helper()
	store, mapping := loadMigrated(t, dir)
	alice := store.SessionsFor("user:alice")
	if len(alice) != 3 || len(mapping["user:alice"]) != 3 {
		t.Fatalf("alice sessions = %+v, mapping %+v", alice, mapping)
	}
	seen := map[string]bool{}
	for i, s := range alice {
		if len(s.KlaxID) != ids.Length || seen[s.KlaxID] || s.KlaxID != mapping["user:alice"][i].KlaxID {
			t.Fatalf("session %d klax_id %q (mapping %+v)", i, s.KlaxID, mapping["user:alice"][i])
		}
		seen[s.KlaxID] = true
	}
	if m := mapping["user:alice"]; m[0].Created != 1775462460 || m[1].Created != 1775462460 || m[2].Created != 1783809967 {
		t.Fatalf("mapping lost old numbers: %+v", m)
	}
	w := alice[0]
	if w.BackendID != "89c19fb7" || w.ModelUsed != "claude-sonnet-x" || w.ModelRequested != "sonnet" || w.Think != "high" ||
		!w.TTY || w.ContextWindow != 200000 || w.ReadPos != "42.3" || w.ReadPosRO != "40.1" ||
		w.SystemPrompt != "Answer in Russian." || w.Messages != 3 || len(w.Groups) != 1 || !w.Active {
		t.Fatalf("converted session = %+v", w)
	}
	if alice[1].Think != "low" {
		t.Fatalf("effort_override not folded: %+v", alice[1])
	}
	def := store.ScopeDefaults("user:alice")
	if def.ModelRequested != "opus" || !def.TTY || def.Think != "high" {
		t.Fatalf("scope defaults = %+v", def)
	}
	if mode := store.ScopeDefaults("ym:g#t").GroupAttachmentMode; mode != "any" {
		t.Fatalf("group_attachments not folded: %q", mode)
	}
	// The first of two sessions sharing a number owns its directory; the second and the session
	// without a directory get none.
	if _, err := os.Stat(WorkDir("user:alice", alice[0].KlaxID)); err != nil {
		t.Fatal(err)
	}
	for _, s := range alice[1:] {
		if _, err := os.Stat(WorkDir("user:alice", s.KlaxID)); !os.IsNotExist(err) {
			t.Fatalf("%s: unexpected dir: %v", s.Name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", session.KeyDir("user:alice"), "1775462460")); !os.IsNotExist(err) {
		t.Fatalf("old dir left: %v", err)
	}
	q, err := os.ReadFile(filepath.Join(WorkDir("user:alice", alice[0].KlaxID), "queue.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(q)
	if strings.Contains(text, `"seq":1`) || strings.Contains(text, `"session"`) || !strings.Contains(text, `"turn_seq":1`) ||
		!strings.Contains(text, `"backend_id":"89c19fb7"`) || !strings.Contains(text, `"ts":1791059329415329941`) ||
		!strings.HasSuffix(text, "\n{\"ev\":\"enq\",\"seq\":2,\"te") {
		t.Fatalf("queue = %s", text)
	}
	turns, err := Open("user:alice", alice[0].KlaxID).InboundLog()
	if err != nil || len(turns) != 1 || turns[0].Seq != 1 || turns[0].BackendID != "89c19fb7" || turns[0].Last != "done" {
		t.Fatalf("replayed turns = %+v, %v", turns, err)
	}
}

func TestMigrateStore(t *testing.T) {
	dir := legacyFixture(t)
	if _, err := session.LoadStore(); err != session.ErrLegacyStore {
		t.Fatalf("LoadStore on a legacy store = %v, want ErrLegacyStore", err)
	}
	if err := MigrateStore(); err != nil {
		t.Fatal(err)
	}
	checkMigrated(t, dir)
	if err := MigrateStore(); err != nil {
		t.Fatalf("second run on a migrated store: %v", err)
	}
	checkMigrated(t, dir)
}

// A crash after any step leaves the journal; the next run resumes from it.
func TestMigrateStoreResumes(t *testing.T) {
	for _, stop := range []string{"journal", "dirs", "queues", "store"} {
		t.Run(stop, func(t *testing.T) {
			dir := legacyFixture(t)
			data, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
			var old legacyStore
			json.Unmarshal(data, &old)
			mapping := newMapping(old)
			buf, _ := json.Marshal(mapping)
			if err := writeDurable(filepath.Join(dir, "sessions.migrate.json"), buf); err != nil {
				t.Fatal(err)
			}
			if stop != "journal" {
				if err := migrateDirs(mapping); err != nil {
					t.Fatal(err)
				}
			}
			if stop == "queues" || stop == "store" {
				if err := migrateQueues(mapping); err != nil {
					t.Fatal(err)
				}
			}
			if stop == "store" {
				out, _ := json.Marshal(convertStore(old, mapping))
				if err := writeDurable(filepath.Join(dir, "sessions.json"), out); err != nil {
					t.Fatal(err)
				}
			}
			if err := MigrateStore(); err != nil {
				t.Fatal(err)
			}
			checkMigrated(t, dir)
		})
	}
}

func TestMigrateStoreRefusesToOverwrite(t *testing.T) {
	dir := legacyFixture(t)
	data, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	var old legacyStore
	json.Unmarshal(data, &old)
	mapping := newMapping(old)
	buf, _ := json.Marshal(mapping)
	writeDurable(filepath.Join(dir, "sessions.migrate.json"), buf)
	if err := os.MkdirAll(WorkDir("mx:1", mapping["mx:1"][0].KlaxID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := MigrateStore(); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("MigrateStore with source and target = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.migrate.json")); err != nil {
		t.Fatalf("journal must stay for a retry: %v", err)
	}
}

func TestMigrateStoreScopeOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", dir)
	os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(`{"chats":{},"scope_defaults":{"user:a":{"model":"opus","claude_tty":true}}}`), 0600)
	if err := MigrateStore(); err != nil {
		t.Fatal(err)
	}
	store, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	if def := store.ScopeDefaults("user:a"); def.ModelRequested != "opus" || !def.TTY {
		t.Fatalf("scope defaults = %+v", def)
	}
}
